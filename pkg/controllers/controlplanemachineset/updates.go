/*
Copyright 2022 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controlplanemachineset

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	machinev1 "github.com/openshift/api/machine/v1"
	"github.com/openshift/cluster-control-plane-machine-set-operator/pkg/machineproviders"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// createdReplacement is a log message used to inform the user that a new Machine was created to
	// replace an existing Machine.
	createdReplacement = "Created replacement machine"

	// errorCreatingMachine is a log message used to inform the user that an error occurred while
	// attempting to create a replacement Machine.
	errorCreatingMachine = "Error creating machine"

	// errorDeletingMachine is a log message used to inform the user that an error occurred while
	// attempting to delete replacement Machine.
	errorDeletingMachine = "Error deleting machine"

	// invalidStrategyMessage is used to inform the user that they have provided an invalid value
	// for the update strategy.
	invalidStrategyMessage = "invalid value for spec.strategy.type"

	// machineRequiresUpdate is a log message used to inform the user that a Machine requires an update,
	// but that they must first delete the Machine to trigger a replacement.
	// This is used with the OnDelete replacement strategy.
	machineRequiresUpdate = "Machine requires an update, delete the machine to trigger a replacement"

	// noUpdatesRequired is a log message used to inform the user that no updates are required within
	// the current set of Machines.
	noUpdatesRequired = "No updates required"

	// removingOldMachine is a log message used to inform the user that an old Machine has been
	// deleted as a part of the rollout operation.
	removingOldMachine = "Removing old machine"

	// waitingForReady is a log message used to inform the user that no operations are taking
	// place because the rollout is waiting for a Machine to be ready.
	// This is used exclusively when adding a new Machine to a missing index.
	waitingForReady = "Waiting for machine to become ready"

	// waitingForRemoved is a log message used to inform the user that no operations are taking
	// place because the rollout is waiting for a Machine to be removed.
	waitingForRemoved = "Waiting for machine to be removed"

	// waitingForReplacement is a log message used to inform the user that no operations are taking
	// place because the rollout is waiting for a replacement Machine to become ready.
	// This is used when replacing a Machine within an index.
	waitingForReplacement = "Waiting for replacement machine to become ready"
)

var (
	// errRecreateStrategyNotSupported is used to inform users that the Recreate update strategy is not yet supported.
	// It may be supported in a future version.
	errRecreateStrategyNotSupported = fmt.Errorf("update strategy %q is not supported", machinev1.Recreate)

	// errReplicasRequired is used to inform users that the replicas field is currently unset, and
	// must be set to continue operation.
	errReplicasRequired = errors.New("spec.replicas is unset: replicas is required")

	// errUnknownStrategy is used to inform users that the update strategy they have provided is not recognised.
	errUnknownStrategy = errors.New("unknown update strategy")
)

// reconcileMachineUpdates determines if any Machines are in need of an update and then handles those updates as per the
// update strategy within the ControlPlaneMachineSet.
// When a Machine needs an update, this function should create a replacement where appropriate.
func (r *ControlPlaneMachineSetReconciler) reconcileMachineUpdates(ctx context.Context, logger logr.Logger, cpms *machinev1.ControlPlaneMachineSet, machineProvider machineproviders.MachineProvider, machineInfos map[int32][]machineproviders.MachineInfo) (ctrl.Result, error) {
	switch cpms.Spec.Strategy.Type {
	case machinev1.RollingUpdate:
		return r.reconcileMachineRollingUpdate(ctx, logger, cpms, machineProvider, machineInfos)
	case machinev1.OnDelete:
		return r.reconcileMachineOnDeleteUpdate(ctx, logger, cpms, machineProvider, machineInfos)
	case machinev1.Recreate:
		meta.SetStatusCondition(&cpms.Status.Conditions, metav1.Condition{
			Type:    conditionDegraded,
			Status:  metav1.ConditionTrue,
			Reason:  reasonInvalidStrategy,
			Message: fmt.Sprintf("%s: %s", invalidStrategyMessage, errRecreateStrategyNotSupported),
		})

		logger.Error(errRecreateStrategyNotSupported, invalidStrategyMessage)
	default:
		meta.SetStatusCondition(&cpms.Status.Conditions,
			metav1.Condition{
				Type:    conditionDegraded,
				Status:  metav1.ConditionTrue,
				Reason:  reasonInvalidStrategy,
				Message: fmt.Sprintf("%s: %s: %s", invalidStrategyMessage, errUnknownStrategy, cpms.Spec.Strategy.Type),
			})

		logger.Error(fmt.Errorf("%w: %s", errUnknownStrategy, cpms.Spec.Strategy.Type), invalidStrategyMessage)
	}

	// Do not return an error here as we only return here when the strategy is invalid.
	// This will need user intervention to resolve.
	return ctrl.Result{}, nil
}

// reconcileMachineRollingUpdate implements the rolling update strategy for the ControlPlaneMachineSet. It uses the
// indexed machine information to determine when a new Machine is required to be created. When a new Machine is required,
// it uses the machine provider to create the new Machine.
//
// For rolling updates, a new Machine is required when a machine index has a Machine, which needs an update, but does
// not yet have replacement created. It must also observe the surge semantics of a rolling update, so, if an existing
// index is already going through the process of a rolling update, it should not start the update of any other index.
// At present, the surge is limited to a single Machine instance.
//
// Once a replacement Machine is ready, the strategy should also delete the old Machine to allow it to be removed from
// the cluster.
//
// In certain scenarios, there may be indexes with missing Machines. In these circumstances, the update should attempt
// to create a new Machine to fulfil the requirement of that index.
func (r *ControlPlaneMachineSetReconciler) reconcileMachineRollingUpdate(ctx context.Context, logger logr.Logger, cpms *machinev1.ControlPlaneMachineSet, machineProvider machineproviders.MachineProvider, indexedMachineInfos map[int32][]machineproviders.MachineInfo) (ctrl.Result, error) {
	logger = logger.WithValues("updateStrategy", cpms.Spec.Strategy.Type)

	// To ensure an ordered and safe reconciliation,
	// one index at a time is considered.
	// Indexes are sorted in ascendent order, so that all the operations of the same importance,
	// are executed prioritizing the lower indexes first.
	sortedIndexedMs := sortMachineInfos(indexedMachineInfos)

	// The maximum number of machines that
	// can be scheduled above the original number of desired machines.
	// At present, the surge is limited to a single Machine instance.
	maxSurge := 1
	// Devise the existing surge and keep track of the current surge count.
	// No check for early stoppage is done here,
	// as deletions can continue even if the maxSurge has been already reached.
	surgeCount := deviseExistingSurge(cpms, sortedIndexedMs)

	// Reconcile any index with no Machine first.
	for idx, machines := range sortedIndexedMs {
		if empty(machines) {
			// There are No Machines for this index.
			// Create a new Machine for it.
			logger = logger.WithValues("index", idx, "namespace", r.Namespace, "name", "<Unknown>")
			return createMachine(ctx, logger, machineProvider, idx, maxSurge, &surgeCount)
		}
	}

	// Reconcile any index with no Ready Machines but a replacement pending.
	for idx, machines := range sortedIndexedMs {
		// Find out if and what Machines in this index need an update.
		machinesPending := pendingMachines(machines)
		if empty(readyMachines(machines)) && hasAny(machinesPending) {
			// There are No Ready Machines for this index but a Pending Machine Replacement is present.
			// Wait for it to become Ready.
			// Consider the first found pending machine for this index to be the replacement machine.
			replacementMachine := machinesPending[0]
			logger = logger.WithValues("index", idx, "namespace", r.Namespace, "name", replacementMachine.MachineRef.ObjectMeta.Name)
			logger.V(2).Info(waitingForReady)
			return ctrl.Result{}, nil
		}
	}

	// Reconcile machines that need an update.
	for idx, machines := range sortedIndexedMs {
		// Find out if and what Machines in this index need an update.
		outdatedMs := needUpdateMachines(machines)
		if hasAny(outdatedMs) {
			// Some Machines need an update for this index.
			// For this reconciliation, just consider the first Machine that needs update for this index.
			outdatedMachine := outdatedMs[0]
			logger = logger.WithValues("index", idx, "namespace", r.Namespace, "name", outdatedMachine.MachineRef.ObjectMeta.Name)

			// Check if an Updated (Spec up-to-date and Ready) Machine replacement already exists for this index.
			if hasAny(updatedMachines(machines)) {
				// A replacement exists.
				if !isDeletedMachine(outdatedMachine) {
					// The Outdated Machine is still around.
					// Now that an Updated replacement exists for it,
					// it's safe to trigger its Deletion.
					return deleteMachine(ctx, logger, machineProvider, outdatedMachine, r.Namespace, idx)
				}

				// The Outdated Machine has already been marked for deletion.
				// Wait for its removal.
				logger.V(2).Info(waitingForRemoved)
				return ctrl.Result{}, nil
			}

			// Check if a Pending (Spec up-to-date but Non Ready) Replacement is present for the index.
			machinesPending := pendingMachines(machines)
			if hasAny(machinesPending) {
				// A Pending Machine Replacement is present.
				// Wait for it to become Ready.
				// Consider the first found pending machine for this index to be the replacement machine.
				replacementMachine := machinesPending[0]
				logger.V(2).WithValues("replacementName", replacementMachine.MachineRef.ObjectMeta.Name).Info(waitingForReplacement)
				return ctrl.Result{}, nil
			}

			// No Healthy or Pending Replacement Machine exists,
			// trigger a Machine creation.
			return createMachine(ctx, logger, machineProvider, idx, maxSurge, &surgeCount)
		}
	}

	// If here it means no updates were required.
	logger.V(4).Info(noUpdatesRequired)

	return ctrl.Result{}, nil
}

// reconcileMachineOnDeleteUpdate implements the rolling update strategy for the ControlPlaneMachineSet. It uses the
// indexed machine information to determine when a new Machine is required to be created. When a new Machine is required,
// it uses the machine provider to create the new Machine.
//
// For on-delete updates, a new Machine is required when a machine index has a Machine with a non-zero deletion
// timestamp but does not yet have a replacement created.
//
// In certain scenarios, there may be indexes with missing Machines. In these circumstances, the update should attempt
// to create a new Machine to fulfil the requirement of that index.
func (r *ControlPlaneMachineSetReconciler) reconcileMachineOnDeleteUpdate(ctx context.Context, logger logr.Logger, cpms *machinev1.ControlPlaneMachineSet, machineProvider machineproviders.MachineProvider, indexedMachineInfos map[int32][]machineproviders.MachineInfo) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// isDeletedMachine checks if a machine is deleted.
func isDeletedMachine(m machineproviders.MachineInfo) bool {
	return m.MachineRef.ObjectMeta.DeletionTimestamp != nil
}

// needUpdateMachines returns the list of MachineInfo which have Machines that need an update.
func needUpdateMachines(machinesInfo []machineproviders.MachineInfo) []machineproviders.MachineInfo {
	needUpdate := []machineproviders.MachineInfo{}

	for _, m := range machinesInfo {
		if m.NeedsUpdate {
			needUpdate = append(needUpdate, m)
		}
	}

	return needUpdate
}

// pendingMachines returns the list of MachineInfo which have a Pending Machine.
func pendingMachines(machinesInfo []machineproviders.MachineInfo) []machineproviders.MachineInfo {
	result := []machineproviders.MachineInfo{}
	for i := range machinesInfo {
		if !machinesInfo[i].Ready && !machinesInfo[i].NeedsUpdate {
			result = append(result, machinesInfo[i])
		}
	}
	return result
}

// updatedMachines returns the list of MachineInfo which have an Updated (Spec up-to-date and Ready) Machine.
func updatedMachines(machinesInfo []machineproviders.MachineInfo) []machineproviders.MachineInfo {
	result := []machineproviders.MachineInfo{}
	for i := range machinesInfo {
		if machinesInfo[i].Ready && !machinesInfo[i].NeedsUpdate {
			result = append(result, machinesInfo[i])
		}
	}
	return result
}

// readyMachines returns the list of MachineInfo which have a Ready Machine.
func readyMachines(machinesInfo []machineproviders.MachineInfo) []machineproviders.MachineInfo {
	result := []machineproviders.MachineInfo{}
	for i := range machinesInfo {
		if machinesInfo[i].Ready {
			result = append(result, machinesInfo[i])
		}
	}
	return result
}

// sortMachineInfos returns a list numerically sorted by index, of each index' MachineInfos.
func sortMachineInfos(indexedMachineInfos map[int32][]machineproviders.MachineInfo) [][]machineproviders.MachineInfo {
	slice := [][]machineproviders.MachineInfo{}

	keys := make([]int, 0, len(indexedMachineInfos))
	for k := range indexedMachineInfos {
		keys = append(keys, int(k))
	}

	sort.Ints(keys)

	for i := range keys {
		slice = append(slice, indexedMachineInfos[int32(i)])
	}
	return slice
}

// deviseExistingSurge computes the current amount of replicas surge for the ControlPlaneMachineSet.
func deviseExistingSurge(cpms *machinev1.ControlPlaneMachineSet, mis [][]machineproviders.MachineInfo) int {
	desiredReplicas := int(*cpms.Spec.Replicas)
	currentReplicas := 0

	for _, mi := range mis {
		currentReplicas += len(mi)
	}

	return currentReplicas - desiredReplicas
}

// hasAny checks if a MachineInfo slice contains at least 1 element.
func hasAny(machinesInfo []machineproviders.MachineInfo) bool {
	return len(machinesInfo) > 0
}

// any checks if a MachineInfo slice is empty.
func empty(machinesInfo []machineproviders.MachineInfo) bool {
	return len(machinesInfo) == 0
}

// deleteMachine deletes the Machine provided.
func deleteMachine(ctx context.Context, logger logr.Logger, machineProvider machineproviders.MachineProvider, outdatedMachine machineproviders.MachineInfo, namespace string, idx int) (ctrl.Result, error) {
	if err := machineProvider.DeleteMachine(ctx, logger, outdatedMachine.MachineRef); err != nil {
		werr := fmt.Errorf("error deleting Machine %s/%s: %w", namespace, outdatedMachine.MachineRef.ObjectMeta.Name, err)
		logger.Error(werr, errorDeletingMachine)
		return ctrl.Result{}, werr
	}

	logger.V(2).Info(removingOldMachine)
	return ctrl.Result{}, nil
}

// createMachine creates the Machine provided.
func createMachine(ctx context.Context, logger logr.Logger, machineProvider machineproviders.MachineProvider, idx int, maxSurge int, surgeCount *int) (ctrl.Result, error) {
	// Check if a surge in Machines is allowed.
	if *surgeCount < maxSurge {
		// There is still room to surge,
		// trigger a Replacement Machine creation.
		if err := machineProvider.CreateMachine(ctx, logger, int32(idx)); err != nil {
			werr := fmt.Errorf("error creating new Machine for index %d: %w", idx, err)
			logger.Error(werr, errorCreatingMachine)
			return ctrl.Result{}, werr
		}

		logger.V(2).Info(createdReplacement)
		*surgeCount++
	}

	return ctrl.Result{}, nil
}
