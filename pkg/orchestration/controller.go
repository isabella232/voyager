package orchestration

import (
	"context"
	"fmt"
	"strings"

	"github.com/atlassian/ctrl"
	cond_v1 "github.com/atlassian/ctrl/apis/condition/v1"
	smith_v1 "github.com/atlassian/smith/pkg/apis/smith/v1"
	smithClient_v1 "github.com/atlassian/smith/pkg/client/clientset_generated/clientset/typed/smith/v1"
	"github.com/atlassian/voyager"
	orch_meta "github.com/atlassian/voyager/pkg/apis/orchestration/meta"
	orch_v1 "github.com/atlassian/voyager/pkg/apis/orchestration/v1"
	"github.com/atlassian/voyager/pkg/k8s"
	"github.com/atlassian/voyager/pkg/k8s/updater"
	orch_v1client "github.com/atlassian/voyager/pkg/orchestration/client/typed/orchestration/v1"
	"github.com/atlassian/voyager/pkg/orchestration/wiring"
	"github.com/atlassian/voyager/pkg/util/layers"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	core_v1 "k8s.io/api/core/v1"
	api_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

const (
	ByConfigMapNameIndexName    = "configMapNamespace"
	ReasonStatusRetrievalFailed = "StatusRetrievalFailed"
)

func ByConfigMapNameIndex(obj interface{}) ([]string, error) {
	state := obj.(*orch_v1.State)
	namespace := state.GetNamespace()
	configMapName := state.Spec.ConfigMapName

	return []string{ByConfigMapNameIndexKey(namespace, configMapName)}, nil
}

func ByConfigMapNameIndexKey(namespace string, configMapName string) string {
	return namespace + "/" + configMapName
}

type Entangler interface {
	Entangle(*orch_v1.State, *wiring.EntangleContext) wiring.EntangleResult
	Status(*orch_v1.StateResource, *wiring.StatusContext) wiring.StatusResult
}

type Controller struct {
	Logger       *zap.Logger
	Clock        clock.Clock
	ReadyForWork func()

	NamespaceInformer cache.SharedIndexInformer
	StateInformer     cache.SharedIndexInformer
	BundleInformer    cache.SharedIndexInformer
	ConfigMapInformer cache.SharedIndexInformer
	StateClient       orch_v1client.StatesGetter
	BundleClient      smithClient_v1.BundlesGetter

	StateTransitionsCounter *prometheus.CounterVec

	Entangler           Entangler
	SpecCheck           updater.SpecCheck
	BundleObjectUpdater updater.ObjectUpdater
}

func (c *Controller) Run(ctx context.Context) {
	defer c.Logger.Info("Shutting down Orchestration controller and rest API")
	c.Logger.Info("Starting the Orchestration controller and rest API")

	c.ReadyForWork()
	<-ctx.Done()
}

func (c *Controller) Process(ctx *ctrl.ProcessContext) (external bool, retriable bool, err error) {
	state := ctx.Object.(*orch_v1.State)
	if state.ObjectMeta.DeletionTimestamp != nil {
		// Marked for deletion, do nothing
		return false, false, nil
	}

	bundle, external, conflict, retriable, err := c.process(ctx.Logger, state)
	if conflict || bundle == nil && err == nil {
		return false, false, nil
	}

	conflict, processRetriable, processErr := c.handleProcessResult(ctx.Logger, state, bundle, retriable, err)
	if conflict {
		return false, false, nil
	}

	if err != nil {
		if processErr != nil {
			ctx.Logger.Error("Failed to set State status", zap.Error(processErr))
		}
		return external, processRetriable || retriable, err
	}

	return false, processRetriable, processErr
}

// process processes the given State object performing autowiring for it.
// It tries to return a Bundle even if there was an error.
func (c *Controller) process(logger *zap.Logger, state *orch_v1.State) (*smith_v1.Bundle, bool /* external */, bool /* conflict */, bool /* retriable */, error) {
	entanglerContext, external, retriable, err := c.constructEntanglerContext(state)
	if err != nil {
		return nil, external, false, retriable, err
	}

	bundle, external, retriable, err := c.entangle(state, entanglerContext)
	if err != nil {
		return nil, external, false, retriable, err
	}

	conflict, retriable, bundle, err := c.createOrUpdateBundle(logger, state, bundle)
	return bundle, false, conflict, retriable, err

}

func (c *Controller) constructEntanglerContext(state *orch_v1.State) (*wiring.EntangleContext, bool /* externalErr */, bool /* retriable */, error) {
	// Grab the namespace
	namespaceObj, exists, err := c.NamespaceInformer.GetIndexer().GetByKey(state.Namespace)
	if err != nil {
		// Should not happen
		return nil, false, false, errors.WithStack(err)
	}
	if !exists {
		// Namespace was deleted while we are processing. Should not happen.
		return nil, false, false, errors.Errorf("missing Namespace %q in informer", state.Namespace)
	}
	namespace := namespaceObj.(*core_v1.Namespace)

	key := ByConfigMapNameIndexKey(state.Namespace, state.Spec.ConfigMapName)
	configMapInterface, exists, err := c.ConfigMapInformer.GetIndexer().GetByKey(key)
	if err != nil {
		// Should not happen
		return nil, false, false, errors.WithStack(err)
	}
	if !exists {
		// This indicates an external error because this means the metadata configmap
		// is missing (the user or layer above has not provided the configmap).
		return nil, true, false, errors.Errorf("missing ConfigMap %q (key: %q) in informer", state.Spec.ConfigMapName, key)
	}
	serviceProperties, err := parseConfigMap(configMapInterface.(*core_v1.ConfigMap))
	if err != nil {
		return nil, true, false, errors.WithStack(err)
	}

	serviceName, err := layers.ServiceNameFromNamespaceLabels(namespace.Labels)
	if err != nil {
		// User error too, we expect the namespace to contain the service name label.
		return nil, true, false, err
	}

	// Entangle the State
	return &wiring.EntangleContext{
		ServiceName:       serviceName,
		Label:             layers.ServiceLabelFromNamespaceLabels(namespace.Labels),
		ServiceProperties: *serviceProperties,
	}, false, false, nil
}

func (c *Controller) entangle(state *orch_v1.State, entangleContext *wiring.EntangleContext) (*smith_v1.Bundle, bool /* external */, bool /* retriable */, error) {
	result := c.Entangler.Entangle(state, entangleContext)
	switch r := result.(type) {
	case *wiring.EntangleResultSuccess:
		return r.Bundle, false, false, nil
	case *wiring.EntangleResultFailure:
		return nil, r.IsExternalError, r.IsRetriableError, errors.Wrapf(r.Error, "failed to wire up Bundle for State %q", state.Name)
	default:
		return nil, false, false, errors.Errorf("unknown entangler state %q", r.StatusType())
	}
}

func (c *Controller) createOrUpdateBundle(logger *zap.Logger, state *orch_v1.State, bundleSpec *smith_v1.Bundle) (conflictRet, retriableRet bool, b *smith_v1.Bundle, e error) {
	conflict, retriable, bundle, err := c.BundleObjectUpdater.CreateOrUpdate(
		logger,
		func(r runtime.Object) error {
			meta := r.(meta_v1.Object)
			if !meta_v1.IsControlledBy(meta, state) {
				return errors.Errorf("Bundle %q is not owned by State %q", meta.GetName(), state.GetName())
			}
			return nil
		},
		bundleSpec,
	)
	// Return the Bundle even if there was an error. The caller might use it to inspect the resource statuses.
	var retBundle *smith_v1.Bundle
	if err == nil {
		retBundle = bundle.(*smith_v1.Bundle)
	} else {
		retBundle = bundleSpec
	}
	return conflict, retriable, retBundle, err
}

func parseConfigMap(configMap *core_v1.ConfigMap) (*orch_meta.ServiceProperties, error) {
	configMapConfigData, ok := configMap.BinaryData[orch_meta.ConfigMapConfigKey]
	if !ok {
		dataAsString, ok := configMap.Data[orch_meta.ConfigMapConfigKey]
		if !ok {
			return nil, errors.Errorf("ConfigMap does not contain expected field %q", orch_meta.ConfigMapConfigKey)
		}
		configMapConfigData = []byte(dataAsString)
	}

	serviceProperties := &orch_meta.ServiceProperties{}
	// If we introduce new fields in the contents of that key in the ConfigMap we still want
	// it to be parseable by old versions of the controller to avoid breaking it.
	// That is why we don't use UnmarshalStrict() here.
	err := yaml.Unmarshal(configMapConfigData, serviceProperties)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return serviceProperties, nil
}

func copyCondition(bundle *smith_v1.Bundle, condType cond_v1.ConditionType, cond *cond_v1.Condition) {
	_, bundleCond := cond_v1.FindCondition(bundle.Status.Conditions, condType)

	if bundleCond == nil {
		cond.Status = cond_v1.ConditionUnknown
		cond.Reason = "SmithInteropError"
		cond.Message = "Smith not reporting state for this condition"
		return
	}

	if bundleCond.Reason != "" {
		cond.Reason = bundleCond.Reason
	}
	if bundleCond.Message != "" {
		cond.Message = "Smith: " + bundleCond.Message
	}
	switch bundleCond.Status {
	case cond_v1.ConditionTrue:
		cond.Status = cond_v1.ConditionTrue
	case cond_v1.ConditionUnknown:
		cond.Status = cond_v1.ConditionUnknown
	case cond_v1.ConditionFalse:
		cond.Status = cond_v1.ConditionFalse
	default:
		cond.Status = cond_v1.ConditionUnknown
		cond.Reason = "SmithInteropError"
		cond.Message = fmt.Sprintf("Unexpected ConditionStatus %q", bundleCond.Status)
	}
}

// handleProcessResult takes the the results of processing and writes it into the
// status of the State. it does not returns the passed in error, thus allowing the
// caller to distinguish between "Status Update Failed" vs "This was the error I passed in"
func (c *Controller) handleProcessResult(logger *zap.Logger, state *orch_v1.State, bundle *smith_v1.Bundle, retriable bool, err error) (conflictRet, retriableRet bool, e error) {
	inProgressCond := cond_v1.Condition{
		Type:   cond_v1.ConditionInProgress,
		Status: cond_v1.ConditionFalse,
	}
	readyCond := cond_v1.Condition{
		Type:   cond_v1.ConditionReady,
		Status: cond_v1.ConditionFalse,
	}
	errorCond := cond_v1.Condition{
		Type:   cond_v1.ConditionError,
		Status: cond_v1.ConditionFalse,
	}
	resourceStatuses := state.Status.ResourceStatuses

	switch {
	case err != nil:
		errorCond.Status = cond_v1.ConditionTrue
		errorCond.Message = err.Error()
		if retriable {
			errorCond.Reason = "RetriableError"
			inProgressCond.Status = cond_v1.ConditionTrue
		} else {
			errorCond.Reason = "TerminalError"
		}
		if bundle != nil { // bundle might be nil in case of an error
			if len(bundle.Status.ResourceStatuses) > 0 {
				// only update resource statuses in State if there is some useful
				// information in Bundle's resource statuses
				resourceStatuses = c.calculateResourceStatuses(state.Spec.Resources, bundle)
			}
		}

	case len(bundle.Status.Conditions) == 0:
		// smith is not currently reporting any Conditions;
		// presumably we've just created something.
		inProgressCond.Status = cond_v1.ConditionTrue
		inProgressCond.Reason = "WaitingOnSmithConditions"
		inProgressCond.Message = "Waiting for Smith to report Conditions (initial creation?)"

	default:
		copyCondition(bundle, smith_v1.BundleInProgress, &inProgressCond)
		copyCondition(bundle, smith_v1.BundleReady, &readyCond)
		copyCondition(bundle, smith_v1.BundleError, &errorCond)

		// The way we calculate these, we assume Smith's status would change
		// if any of the resource statuses cause a transition, so there's no
		// need to recalculate the state condition.
		// However, there is still a need to set the status if the resource
		// status changes (i.e. transition timestamp changes)
		resourceStatuses = c.calculateResourceStatuses(state.Spec.Resources, bundle)
	}

	inProgressUpdated := c.updateCondition(state, inProgressCond)
	readyUpdated := c.updateCondition(state, readyCond)
	errorUpdated := c.updateCondition(state, errorCond)
	resourcesUpdated := c.updateResourceStatuses(state, resourceStatuses)

	// Updating the State status
	if inProgressUpdated || readyUpdated || errorUpdated || resourcesUpdated {
		conflictStatus, retriableStatus, errStatus := c.setStatus(logger, state)
		if errStatus != nil {
			return false, retriableStatus, errStatus
		}
		if conflictStatus {
			return true, false, nil
		}
	}

	return false, false, nil
}

func (c *Controller) calculateResourceStatuses(stateResources []orch_v1.StateResource, bundle *smith_v1.Bundle) []orch_v1.ResourceStatus {
	calculatedResourceStatuses := make([]orch_v1.ResourceStatus, 0, len(stateResources))
	for _, stateRes := range stateResources {
		status := orch_v1.ResourceStatus{
			Name: stateRes.Name,
		}

		result := c.Entangler.Status(&stateRes, prepareStatusContext(stateRes.Name, bundle))
		switch r := result.(type) {
		case *wiring.StatusResultSuccess:
			status.ResourceStatusData = r.ResourceStatusData
		case *wiring.StatusResultFailure:
			// External Errors are not special - all errors are just placed into the status
			status.ResourceStatusData = orch_v1.ResourceStatusData{
				Conditions: []cond_v1.Condition{
					{
						Type:   cond_v1.ConditionInProgress,
						Status: cond_v1.ConditionFalse,
					},
					{
						Type:   cond_v1.ConditionReady,
						Status: cond_v1.ConditionFalse,
					},
					{
						Type:    cond_v1.ConditionError,
						Status:  cond_v1.ConditionTrue,
						Reason:  ReasonStatusRetrievalFailed,
						Message: fmt.Sprintf("Failed to get status of resource: %v", r.Error),
					},
				},
			}
		default:
			status.ResourceStatusData = orch_v1.ResourceStatusData{
				Conditions: []cond_v1.Condition{
					{
						Type:   cond_v1.ConditionInProgress,
						Status: cond_v1.ConditionFalse,
					},
					{
						Type:   cond_v1.ConditionReady,
						Status: cond_v1.ConditionFalse,
					},
					{
						Type:    cond_v1.ConditionError,
						Status:  cond_v1.ConditionTrue,
						Reason:  ReasonStatusRetrievalFailed,
						Message: fmt.Sprintf("Unexpected status result type: %q", r.StatusType()),
					},
				},
			}
		}

		calculatedResourceStatuses = append(calculatedResourceStatuses, status)
	}

	return calculatedResourceStatuses
}

func prepareStatusContext(resourceName voyager.ResourceName, bundle *smith_v1.Bundle) *wiring.StatusContext {
	var resources []wiring.BundleResource

	for _, res := range bundle.Spec.Resources {
		if stateResourceName(res.Name) != resourceName {
			continue
		}
		var status smith_v1.ResourceStatusData
		for _, resStatus := range bundle.Status.ResourceStatuses {
			if resStatus.Name == res.Name {
				status = resStatus.ResourceStatusData
				break
			}
		}
		resources = append(resources, wiring.BundleResource{
			Resource: res,
			Status:   status,
		})
	}
	return &wiring.StatusContext{
		BundleResources: resources,
		PluginStatuses:  bundle.Status.PluginStatuses,
	}
}

// This function relies on the convention for Bundle Resource names documented at
// https://hello.atlassian.net/wiki/spaces/VDEV/pages/154212345/Voyager-Provider+contract#Voyager-Providercontract-BundleResourcenames
func stateResourceName(name smith_v1.ResourceName) voyager.ResourceName {
	n := string(name)
	n = strings.SplitN(n, "--", 2)[0]
	return voyager.ResourceName(n)
}

func (c *Controller) setStatus(logger *zap.Logger, state *orch_v1.State) (conflictRet, retriableRet bool, e error) {
	logger.Info("Writing status")
	_, err := c.StateClient.States(state.Namespace).Update(state)
	if err != nil {
		if api_errors.IsConflict(err) {
			return true, false, nil
		}
		for _, isNonRetriable := range updater.NonRetriableErrors {
			if isNonRetriable(err) {
				return false, false, errors.WithStack(err)
			}
		}
		return false, true, errors.Wrap(err, "failed to set State status")
	}
	return false, false, nil
}

// Updates existing State condition or creates a new one. Sets LastTransitionTime to now if the
// status has changed.
// Returns true if State condition has changed or has been added.
func (c *Controller) updateCondition(s *orch_v1.State, condition cond_v1.Condition) bool {
	var needsUpdate bool
	i, oldCondition := cond_v1.FindCondition(s.Status.Conditions, condition.Type)
	needsUpdate = k8s.FillCondition(c.Clock, oldCondition, &condition)

	if needsUpdate {
		if i == -1 {
			s.Status.Conditions = append(s.Status.Conditions, condition)
		} else {
			s.Status.Conditions[i] = condition
		}
		if condition.Status == cond_v1.ConditionTrue {
			c.StateTransitionsCounter.
				WithLabelValues(s.GetNamespace(), s.GetName(), string(condition.Type), condition.Reason).
				Inc()
		}
		return true
	}

	return false
}

// Updates existing State resource statuses. Returns true if any of the resource
// statuses have changed or been added compared the to previous statuses.
func (c *Controller) updateResourceStatuses(s *orch_v1.State, newResourceStatuses []orch_v1.ResourceStatus) bool {
	// This grabs the existing ResourceStatuses in the State and explodes it into a map of name->resourceStatus
	existing := s.Status.ResourceStatuses
	nameToResourceStatus := make(map[voyager.ResourceName]*orch_v1.ResourceStatus, len(existing))
	for i := range existing {
		nameToResourceStatus[existing[i].Name] = &existing[i]
	}

	// for each of the new resource statuses, check if the state already has it
	newStatuses := make([]orch_v1.ResourceStatus, 0, len(newResourceStatuses))
	var changed bool
	for _, newResourceStatus := range newResourceStatuses {
		existingResourceStatus, hasExistingStatus := nameToResourceStatus[newResourceStatus.Name]
		if hasExistingStatus {
			changed = k8s.FillNewConditions(c.Clock, existingResourceStatus.Conditions, newResourceStatus.Conditions) || changed
		} else {
			changed = k8s.FillNewConditions(c.Clock, nil, newResourceStatus.Conditions) || changed
		}

		newStatuses = append(newStatuses, newResourceStatus)
	}

	if changed {
		s.Status.ResourceStatuses = newStatuses
		return true
	}

	return false
}
