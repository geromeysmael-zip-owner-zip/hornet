package dag

import (
	"bytes"

	"github.com/gohornet/hornet/pkg/model/hornet"
	"github.com/gohornet/hornet/pkg/model/milestone"
	"github.com/gohornet/hornet/pkg/model/tangle"
)

// UpdateOutdatedConeRootIndexes updates the cone root indexes of the given messages.
// the "outdatedMessageIDs" should be ordered from oldest to latest to avoid recursion.
func UpdateOutdatedConeRootIndexes(outdatedMessageIDs hornet.Hashes, lsmi milestone.Index) {
	for _, outdatedMessageID := range outdatedMessageIDs {
		cachedMsgMeta := tangle.GetCachedMessageMetadataOrNil(outdatedMessageID)
		if cachedMsgMeta == nil {
			panic(tangle.ErrMessageNotFound)
		}
		GetConeRootIndexes(cachedMsgMeta, lsmi)
	}
}

// GetConeRootIndexes searches the cone root indexes for a given message.
func GetConeRootIndexes(cachedMsgMeta *tangle.CachedMetadata, lsmi milestone.Index) (youngestConeRootIndex milestone.Index, oldestConeRootIndex milestone.Index) {
	defer cachedMsgMeta.Release(true) // meta -1

	// if the msg already contains recent (calculation index matches LSMI)
	// information about ycri and ocri, return that info
	ycri, ocri, ci := cachedMsgMeta.GetMetadata().GetConeRootIndexes()
	if ci == lsmi {
		return ycri, ocri
	}

	youngestConeRootIndex = 0
	oldestConeRootIndex = 0

	updateIndexes := func(ycri milestone.Index, ocri milestone.Index) {
		if (youngestConeRootIndex == 0) || (youngestConeRootIndex < ycri) {
			youngestConeRootIndex = ycri
		}
		if (oldestConeRootIndex == 0) || (oldestConeRootIndex > ocri) {
			oldestConeRootIndex = ocri
		}
	}

	// collect all parents in the cone that are not confirmed,
	// are no solid entry points and have no recent calculation index
	var outdatedMessageIDs hornet.Hashes

	startMessageID := cachedMsgMeta.GetMetadata().GetMessageID()

	indexesValid := true

	// traverse the parents of this message to calculate the cone root indexes for this message.
	// this walk will also collect all outdated messages in the same cone, to update them afterwards.
	if err := TraverseParents(cachedMsgMeta.GetMetadata().GetMessageID(),
		// traversal stops if no more messages pass the given condition
		// Caution: condition func is not in DFS order
		func(cachedMetadata *tangle.CachedMetadata) (bool, error) { // meta +1
			defer cachedMetadata.Release(true) // meta -1

			// first check if the msg was confirmed => update ycri and ocri with the confirmation index
			if confirmed, at := cachedMetadata.GetMetadata().GetConfirmed(); confirmed {
				updateIndexes(at, at)
				return false, nil
			}

			if bytes.Equal(startMessageID, cachedMetadata.GetMetadata().GetMessageID()) {
				return true, nil
			}

			// if the msg was not confirmed yet, but already contains recent (calculation index matches LSMI) information
			// about ycri and ocri, propagate that info
			ycri, ocri, ci := cachedMetadata.GetMetadata().GetConeRootIndexes()
			if ci == lsmi {
				updateIndexes(ycri, ocri)
				return false, nil
			}

			return true, nil
		},
		// consumer
		func(cachedMetadata *tangle.CachedMetadata) error { // meta +1
			defer cachedMetadata.Release(true) // meta -1

			if bytes.Equal(startMessageID, cachedMetadata.GetMetadata().GetMessageID()) {
				// skip the start message, so it doesn't get added to the outdatedMessageIDs
				return nil
			}

			outdatedMessageIDs = append(outdatedMessageIDs, cachedMetadata.GetMetadata().GetMessageID())
			return nil
		},
		// called on missing parents
		// return error on missing parents
		nil,
		// called on solid entry points
		func(messageID hornet.Hash) {
			// if the parent is a solid entry point, use the index of the solid entry point as ORTSI
			entryPointIndex, _ := tangle.SolidEntryPointsIndex(messageID)
			updateIndexes(entryPointIndex, entryPointIndex)
		}, false, nil); err != nil {
		if err == tangle.ErrMessageNotFound {
			indexesValid = false
		} else {
			panic(err)
		}
	}

	// update the outdated cone root indexes of all messages in the cone in order from oldest msgs to latest.
	// this is an efficient way to update the whole cone, because updating from oldest to latest will not be recursive.
	UpdateOutdatedConeRootIndexes(outdatedMessageIDs, lsmi)

	// only set the calculated cone root indexes if all messages in the past cone were found
	if !indexesValid {
		return 0, 0
	}

	// set the new cone root indexes in the metadata of the message
	cachedMsgMeta.GetMetadata().SetConeRootIndexes(youngestConeRootIndex, oldestConeRootIndex, lsmi)

	return youngestConeRootIndex, oldestConeRootIndex
}

// UpdateConeRootIndexes updates the cone root indexes of the future cone of all given messages.
// all the messages of the newly confirmed cone already have updated cone root indexes.
// we have to walk the future cone, and update the past cone of all messages that reference an old cone.
// as a special property, invocations of the yielded function share the same 'already traversed' set to circumvent
// walking the future cone of the same messages multiple times.
func UpdateConeRootIndexes(messageIDs hornet.Hashes, lsmi milestone.Index) {
	traversed := map[string]struct{}{}

	// we update all messages in order from oldest to latest
	for _, messageID := range messageIDs {

		if err := TraverseChildren(messageID,
			// traversal stops if no more messages pass the given condition
			func(cachedMsgMeta *tangle.CachedMetadata) (bool, error) { // meta +1
				defer cachedMsgMeta.Release(true) // meta -1
				_, previouslyTraversed := traversed[string(cachedMsgMeta.GetMetadata().GetMessageID())]
				return !previouslyTraversed, nil
			},
			// consumer
			func(cachedMsgMeta *tangle.CachedMetadata) error { // meta +1
				defer cachedMsgMeta.Release(true) // meta -1
				traversed[string(cachedMsgMeta.GetMetadata().GetMessageID())] = struct{}{}

				// updates the cone root indexes of the outdated past cone for this message
				GetConeRootIndexes(cachedMsgMeta.Retain(), lsmi) // meta pass +1

				return nil
			}, false, nil); err != nil {
			panic(err)
		}
	}
}