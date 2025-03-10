// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package workers

import (
	"context"
	"errors"

	apimodel "github.com/superseriousbusiness/gotosocial/internal/api/model"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtscontext"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/id"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/processing/account"
	"github.com/superseriousbusiness/gotosocial/internal/processing/media"
	"github.com/superseriousbusiness/gotosocial/internal/state"
	"github.com/superseriousbusiness/gotosocial/internal/uris"
	"github.com/superseriousbusiness/gotosocial/internal/util"
)

// util provides util functions used by both
// the fromClientAPI and fromFediAPI functions.
type utils struct {
	state   *state.State
	media   *media.Processor
	account *account.Processor
	surface *Surface
}

// wipeStatus encapsulates common logic
// used to totally delete a status + all
// its attachments, notifications, boosts,
// and timeline entries.
func (u *utils) wipeStatus(
	ctx context.Context,
	statusToDelete *gtsmodel.Status,
	deleteAttachments bool,
) error {
	var errs gtserror.MultiError

	// Either delete all attachments for this status,
	// or simply unattach + clean them separately later.
	//
	// Reason to unattach rather than delete is that
	// the poster might want to reattach them to another
	// status immediately (in case of delete + redraft)
	if deleteAttachments {
		// todo:u.state.DB.DeleteAttachmentsForStatus
		for _, id := range statusToDelete.AttachmentIDs {
			if err := u.media.Delete(ctx, id); err != nil {
				errs.Appendf("error deleting media: %w", err)
			}
		}
	} else {
		// todo:u.state.DB.UnattachAttachmentsForStatus
		for _, id := range statusToDelete.AttachmentIDs {
			if _, err := u.media.Unattach(ctx, statusToDelete.Account, id); err != nil {
				errs.Appendf("error unattaching media: %w", err)
			}
		}
	}

	// delete all mention entries generated by this status
	// todo:u.state.DB.DeleteMentionsForStatus
	for _, id := range statusToDelete.MentionIDs {
		if err := u.state.DB.DeleteMentionByID(ctx, id); err != nil {
			errs.Appendf("error deleting status mention: %w", err)
		}
	}

	// delete all notification entries generated by this status
	if err := u.state.DB.DeleteNotificationsForStatus(ctx, statusToDelete.ID); err != nil {
		errs.Appendf("error deleting status notifications: %w", err)
	}

	// delete all bookmarks that point to this status
	if err := u.state.DB.DeleteStatusBookmarksForStatus(ctx, statusToDelete.ID); err != nil {
		errs.Appendf("error deleting status bookmarks: %w", err)
	}

	// delete all faves of this status
	if err := u.state.DB.DeleteStatusFavesForStatus(ctx, statusToDelete.ID); err != nil {
		errs.Appendf("error deleting status faves: %w", err)
	}

	if pollID := statusToDelete.PollID; pollID != "" {
		// Delete this poll by ID from the database.
		if err := u.state.DB.DeletePollByID(ctx, pollID); err != nil {
			errs.Appendf("error deleting status poll: %w", err)
		}

		// Delete any poll votes pointing to this poll ID.
		if err := u.state.DB.DeletePollVotes(ctx, pollID); err != nil {
			errs.Appendf("error deleting status poll votes: %w", err)
		}

		// Cancel any scheduled expiry task for poll.
		_ = u.state.Workers.Scheduler.Cancel(pollID)
	}

	// delete all boosts for this status + remove them from timelines
	boosts, err := u.state.DB.GetStatusBoosts(
		// we MUST set a barebones context here,
		// as depending on where it came from the
		// original BoostOf may already be gone.
		gtscontext.SetBarebones(ctx),
		statusToDelete.ID)
	if err != nil {
		errs.Appendf("error fetching status boosts: %w", err)
	}

	for _, boost := range boosts {
		if err := u.surface.deleteStatusFromTimelines(ctx, boost.ID); err != nil {
			errs.Appendf("error deleting boost from timelines: %w", err)
		}
		if err := u.state.DB.DeleteStatusByID(ctx, boost.ID); err != nil {
			errs.Appendf("error deleting boost: %w", err)
		}
	}

	// delete this status from any and all timelines
	if err := u.surface.deleteStatusFromTimelines(ctx, statusToDelete.ID); err != nil {
		errs.Appendf("error deleting status from timelines: %w", err)
	}

	// delete this status from any conversations that it's part of
	if err := u.state.DB.DeleteStatusFromConversations(ctx, statusToDelete.ID); err != nil {
		errs.Appendf("error deleting status from conversations: %w", err)
	}

	// finally, delete the status itself
	if err := u.state.DB.DeleteStatusByID(ctx, statusToDelete.ID); err != nil {
		errs.Appendf("error deleting status: %w", err)
	}

	return errs.Combine()
}

// redirectFollowers redirects all local
// followers of originAcct to targetAcct.
//
// Both accounts must be fully dereferenced
// already, and the Move must be valid.
//
// Return bool will be true if all goes OK.
func (u *utils) redirectFollowers(
	ctx context.Context,
	originAcct *gtsmodel.Account,
	targetAcct *gtsmodel.Account,
) bool {
	// Any local followers of originAcct should
	// send follow requests to targetAcct instead,
	// and have followers of originAcct removed.
	//
	// Select local followers with barebones, since
	// we only need follow.Account and we can get
	// that ourselves.
	followers, err := u.state.DB.GetAccountLocalFollowers(
		gtscontext.SetBarebones(ctx),
		originAcct.ID,
	)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		log.Errorf(ctx,
			"db error getting follows targeting originAcct: %v",
			err,
		)
		return false
	}

	for _, follow := range followers {
		// Fetch the local account that
		// owns the follow targeting originAcct.
		if follow.Account, err = u.state.DB.GetAccountByID(
			gtscontext.SetBarebones(ctx),
			follow.AccountID,
		); err != nil {
			log.Errorf(ctx,
				"db error getting follow account %s: %v",
				follow.AccountID, err,
			)
			return false
		}

		// Use the account processor FollowCreate
		// function to send off the new follow,
		// carrying over the Reblogs and Notify
		// values from the old follow to the new.
		//
		// This will also handle cases where our
		// account has already followed the target
		// account, by just updating the existing
		// follow of target account.
		//
		// Also, ensure new follow wouldn't be a
		// self follow, since that will error.
		if follow.AccountID != targetAcct.ID {
			if _, err := u.account.FollowCreate(
				ctx,
				follow.Account,
				&apimodel.AccountFollowRequest{
					ID:      targetAcct.ID,
					Reblogs: follow.ShowReblogs,
					Notify:  follow.Notify,
				},
			); err != nil {
				log.Errorf(ctx,
					"error creating new follow for account %s: %v",
					follow.AccountID, err,
				)
				return false
			}
		}

		// New follow is in the process of
		// sending, remove the existing follow.
		// This will send out an Undo Activity for each Follow.
		if _, err := u.account.FollowRemove(
			ctx,
			follow.Account,
			follow.TargetAccountID,
		); err != nil {
			log.Errorf(ctx,
				"error removing old follow for account %s: %v",
				follow.AccountID, err,
			)
			return false
		}
	}

	return true
}

func (u *utils) incrementStatusesCount(
	ctx context.Context,
	account *gtsmodel.Account,
	status *gtsmodel.Status,
) error {
	// Lock on this account since we're changing stats.
	unlock := u.state.ProcessingLocks.Lock(account.URI)
	defer unlock()

	// Populate stats.
	if err := u.state.DB.PopulateAccountStats(ctx, account); err != nil {
		return gtserror.Newf("db error getting account stats: %w", err)
	}

	// Update stats by incrementing status
	// count by one and setting last posted.
	*account.Stats.StatusesCount++
	account.Stats.LastStatusAt = status.CreatedAt
	if err := u.state.DB.UpdateAccountStats(
		ctx,
		account.Stats,
		"statuses_count",
		"last_status_at",
	); err != nil {
		return gtserror.Newf("db error updating account stats: %w", err)
	}

	return nil
}

func (u *utils) decrementStatusesCount(
	ctx context.Context,
	account *gtsmodel.Account,
) error {
	// Lock on this account since we're changing stats.
	unlock := u.state.ProcessingLocks.Lock(account.URI)
	defer unlock()

	// Populate stats.
	if err := u.state.DB.PopulateAccountStats(ctx, account); err != nil {
		return gtserror.Newf("db error getting account stats: %w", err)
	}

	// Update stats by decrementing
	// status count by one.
	//
	// Clamp to 0 to avoid funny business.
	*account.Stats.StatusesCount--
	if *account.Stats.StatusesCount < 0 {
		*account.Stats.StatusesCount = 0
	}
	if err := u.state.DB.UpdateAccountStats(
		ctx,
		account.Stats,
		"statuses_count",
	); err != nil {
		return gtserror.Newf("db error updating account stats: %w", err)
	}

	return nil
}

func (u *utils) incrementFollowersCount(
	ctx context.Context,
	account *gtsmodel.Account,
) error {
	// Lock on this account since we're changing stats.
	unlock := u.state.ProcessingLocks.Lock(account.URI)
	defer unlock()

	// Populate stats.
	if err := u.state.DB.PopulateAccountStats(ctx, account); err != nil {
		return gtserror.Newf("db error getting account stats: %w", err)
	}

	// Update stats by incrementing followers
	// count by one and setting last posted.
	*account.Stats.FollowersCount++
	if err := u.state.DB.UpdateAccountStats(
		ctx,
		account.Stats,
		"followers_count",
	); err != nil {
		return gtserror.Newf("db error updating account stats: %w", err)
	}

	return nil
}

func (u *utils) decrementFollowersCount(
	ctx context.Context,
	account *gtsmodel.Account,
) error {
	// Lock on this account since we're changing stats.
	unlock := u.state.ProcessingLocks.Lock(account.URI)
	defer unlock()

	// Populate stats.
	if err := u.state.DB.PopulateAccountStats(ctx, account); err != nil {
		return gtserror.Newf("db error getting account stats: %w", err)
	}

	// Update stats by decrementing
	// followers count by one.
	//
	// Clamp to 0 to avoid funny business.
	*account.Stats.FollowersCount--
	if *account.Stats.FollowersCount < 0 {
		*account.Stats.FollowersCount = 0
	}
	if err := u.state.DB.UpdateAccountStats(
		ctx,
		account.Stats,
		"followers_count",
	); err != nil {
		return gtserror.Newf("db error updating account stats: %w", err)
	}

	return nil
}

func (u *utils) incrementFollowingCount(
	ctx context.Context,
	account *gtsmodel.Account,
) error {
	// Lock on this account since we're changing stats.
	unlock := u.state.ProcessingLocks.Lock(account.URI)
	defer unlock()

	// Populate stats.
	if err := u.state.DB.PopulateAccountStats(ctx, account); err != nil {
		return gtserror.Newf("db error getting account stats: %w", err)
	}

	// Update stats by incrementing
	// followers count by one.
	*account.Stats.FollowingCount++
	if err := u.state.DB.UpdateAccountStats(
		ctx,
		account.Stats,
		"following_count",
	); err != nil {
		return gtserror.Newf("db error updating account stats: %w", err)
	}

	return nil
}

func (u *utils) decrementFollowingCount(
	ctx context.Context,
	account *gtsmodel.Account,
) error {
	// Lock on this account since we're changing stats.
	unlock := u.state.ProcessingLocks.Lock(account.URI)
	defer unlock()

	// Populate stats.
	if err := u.state.DB.PopulateAccountStats(ctx, account); err != nil {
		return gtserror.Newf("db error getting account stats: %w", err)
	}

	// Update stats by decrementing
	// following count by one.
	//
	// Clamp to 0 to avoid funny business.
	*account.Stats.FollowingCount--
	if *account.Stats.FollowingCount < 0 {
		*account.Stats.FollowingCount = 0
	}
	if err := u.state.DB.UpdateAccountStats(
		ctx,
		account.Stats,
		"following_count",
	); err != nil {
		return gtserror.Newf("db error updating account stats: %w", err)
	}

	return nil
}

func (u *utils) incrementFollowRequestsCount(
	ctx context.Context,
	account *gtsmodel.Account,
) error {
	// Lock on this account since we're changing stats.
	unlock := u.state.ProcessingLocks.Lock(account.URI)
	defer unlock()

	// Populate stats.
	if err := u.state.DB.PopulateAccountStats(ctx, account); err != nil {
		return gtserror.Newf("db error getting account stats: %w", err)
	}

	// Update stats by incrementing
	// follow requests count by one.
	*account.Stats.FollowRequestsCount++
	if err := u.state.DB.UpdateAccountStats(
		ctx,
		account.Stats,
		"follow_requests_count",
	); err != nil {
		return gtserror.Newf("db error updating account stats: %w", err)
	}

	return nil
}

func (u *utils) decrementFollowRequestsCount(
	ctx context.Context,
	account *gtsmodel.Account,
) error {
	// Lock on this account since we're changing stats.
	unlock := u.state.ProcessingLocks.Lock(account.URI)
	defer unlock()

	// Populate stats.
	if err := u.state.DB.PopulateAccountStats(ctx, account); err != nil {
		return gtserror.Newf("db error getting account stats: %w", err)
	}

	// Update stats by decrementing
	// follow requests count by one.
	//
	// Clamp to 0 to avoid funny business.
	*account.Stats.FollowRequestsCount--
	if *account.Stats.FollowRequestsCount < 0 {
		*account.Stats.FollowRequestsCount = 0
	}
	if err := u.state.DB.UpdateAccountStats(
		ctx,
		account.Stats,
		"follow_requests_count",
	); err != nil {
		return gtserror.Newf("db error updating account stats: %w", err)
	}

	return nil
}

// approveFave stores + returns an
// interactionApproval for a fave.
func (u *utils) approveFave(
	ctx context.Context,
	fave *gtsmodel.StatusFave,
) (*gtsmodel.InteractionApproval, error) {
	id := id.NewULID()

	approval := &gtsmodel.InteractionApproval{
		ID:                   id,
		AccountID:            fave.TargetAccountID,
		Account:              fave.TargetAccount,
		InteractingAccountID: fave.AccountID,
		InteractingAccount:   fave.Account,
		InteractionURI:       fave.URI,
		InteractionType:      gtsmodel.InteractionLike,
		URI:                  uris.GenerateURIForAccept(fave.TargetAccount.Username, id),
	}

	if err := u.state.DB.PutInteractionApproval(ctx, approval); err != nil {
		err := gtserror.Newf("db error inserting interaction approval: %w", err)
		return nil, err
	}

	// Mark the fave itself as now approved.
	fave.PendingApproval = util.Ptr(false)
	fave.PreApproved = false
	fave.ApprovedByURI = approval.URI

	if err := u.state.DB.UpdateStatusFave(
		ctx,
		fave,
		"pending_approval",
		"approved_by_uri",
	); err != nil {
		err := gtserror.Newf("db error updating status fave: %w", err)
		return nil, err
	}

	return approval, nil
}

// approveReply stores + returns an
// interactionApproval for a reply.
func (u *utils) approveReply(
	ctx context.Context,
	status *gtsmodel.Status,
) (*gtsmodel.InteractionApproval, error) {
	id := id.NewULID()

	approval := &gtsmodel.InteractionApproval{
		ID:                   id,
		AccountID:            status.InReplyToAccountID,
		Account:              status.InReplyToAccount,
		InteractingAccountID: status.AccountID,
		InteractingAccount:   status.Account,
		InteractionURI:       status.URI,
		InteractionType:      gtsmodel.InteractionReply,
		URI:                  uris.GenerateURIForAccept(status.InReplyToAccount.Username, id),
	}

	if err := u.state.DB.PutInteractionApproval(ctx, approval); err != nil {
		err := gtserror.Newf("db error inserting interaction approval: %w", err)
		return nil, err
	}

	// Mark the status itself as now approved.
	status.PendingApproval = util.Ptr(false)
	status.PreApproved = false
	status.ApprovedByURI = approval.URI

	if err := u.state.DB.UpdateStatus(
		ctx,
		status,
		"pending_approval",
		"approved_by_uri",
	); err != nil {
		err := gtserror.Newf("db error updating status: %w", err)
		return nil, err
	}

	return approval, nil
}

// approveAnnounce stores + returns an
// interactionApproval for an announce.
func (u *utils) approveAnnounce(
	ctx context.Context,
	boost *gtsmodel.Status,
) (*gtsmodel.InteractionApproval, error) {
	id := id.NewULID()

	approval := &gtsmodel.InteractionApproval{
		ID:                   id,
		AccountID:            boost.BoostOfAccountID,
		Account:              boost.BoostOfAccount,
		InteractingAccountID: boost.AccountID,
		InteractingAccount:   boost.Account,
		InteractionURI:       boost.URI,
		InteractionType:      gtsmodel.InteractionReply,
		URI:                  uris.GenerateURIForAccept(boost.BoostOfAccount.Username, id),
	}

	if err := u.state.DB.PutInteractionApproval(ctx, approval); err != nil {
		err := gtserror.Newf("db error inserting interaction approval: %w", err)
		return nil, err
	}

	// Mark the status itself as now approved.
	boost.PendingApproval = util.Ptr(false)
	boost.PreApproved = false
	boost.ApprovedByURI = approval.URI

	if err := u.state.DB.UpdateStatus(
		ctx,
		boost,
		"pending_approval",
		"approved_by_uri",
	); err != nil {
		err := gtserror.Newf("db error updating boost wrapper status: %w", err)
		return nil, err
	}

	return approval, nil
}
