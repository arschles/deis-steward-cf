package claim

import (
	"errors"
	"fmt"

	"github.com/deis/steward/k8s"
	"github.com/deis/steward/mode"
	"github.com/pborman/uuid"
	"golang.org/x/net/context"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	kcl "k8s.io/kubernetes/pkg/client/unversioned"
)

var (
	errMissingInstanceID = errors.New("missing instance ID")
	errMissingBindID     = errors.New("missing bind ID")
)

type claimUpdate struct {
	newClaim *mode.ServicePlanClaim
	err      error
	stop     bool
}

func newErrClaimUpdate(claim *mode.ServicePlanClaim, err error) claimUpdate {
	claim.Status = mode.StatusFailed.String()
	claim.StatusDescription = fmt.Sprintf("error: %s", err)
	return claimUpdate{
		newClaim: claim,
		err:      err,
		stop:     true,
	}
}

func newClaimUpdate(claim *mode.ServicePlanClaim) claimUpdate {
	stop := false
	if mode.StringIsStatus(claim.Status, mode.StatusFailed) ||
		mode.StringIsStatus(claim.Status, mode.StatusBound) ||
		mode.StringIsStatus(claim.Status, mode.StatusProvisioned) ||
		mode.StringIsStatus(claim.Status, mode.StatusUnbound) ||
		mode.StringIsStatus(claim.Status, mode.StatusDeprovisioned) {
		stop = true
	}

	return claimUpdate{newClaim: claim, stop: stop}
}

type errNoSuchServiceAndPlan struct {
	svcID  string
	planID string
}

func (e errNoSuchServiceAndPlan) Error() string {
	return fmt.Sprintf("no such service and plan. service ID = %s, plan ID = %s", e.svcID, e.planID)
}

func isNoSuchServiceAndPlanErr(e error) bool {
	_, ok := e.(errNoSuchServiceAndPlan)
	return ok
}

func getService(claim *mode.ServicePlanClaim, catalog k8s.ServiceCatalogLookup) (*k8s.ServiceCatalogEntry, error) {
	svc := catalog.Get(claim.ServiceID, claim.PlanID)
	if svc == nil {
		logger.Debugf("service %s, plan %s not found", claim.ServiceID, claim.PlanID)
		return nil, errNoSuchServiceAndPlan{
			svcID:  claim.ServiceID,
			planID: claim.PlanID,
		}
	}
	return svc, nil
}

func processProvision(
	ctx context.Context,
	evt *Event,
	cmNamespacer kcl.ConfigMapsNamespacer,
	catalogLookup k8s.ServiceCatalogLookup,
	lifecycler mode.Lifecycler,
	claimCh chan<- claimUpdate,
) {

	claim := evt.claim.Claim

	svc, err := getService(claim, catalogLookup)
	if err != nil {
		select {
		case claimCh <- newErrClaimUpdate(claim, err):
		case <-ctx.Done():
		}
		return
	}

	claim.Status = mode.StatusProvisioning.String()
	select {
	case claimCh <- newClaimUpdate(claim):
	case <-ctx.Done():
		return
	}
	orgGUID := uuid.New()
	spaceGUID := uuid.New()
	instanceID := uuid.New()
	claim.InstanceID = instanceID
	if _, err := lifecycler.Provision(instanceID, &mode.ProvisionRequest{
		OrganizationGUID: orgGUID,
		PlanID:           svc.Plan.ID,
		ServiceID:        svc.Info.ID,
		SpaceGUID:        spaceGUID,
		Parameters:       mode.JSONObject(map[string]string{}),
	}); err != nil {
		select {
		case claimCh <- newErrClaimUpdate(claim, err):
		case <-ctx.Done():
		}
		return
	}

	claim.Status = mode.StatusProvisioned.String()
	select {
	case claimCh <- newClaimUpdate(claim):
	case <-ctx.Done():
		return
	}
}

func processBind(
	ctx context.Context,
	evt *Event,
	cmNamespacer kcl.ConfigMapsNamespacer,
	catalogLookup k8s.ServiceCatalogLookup,
	lifecycler mode.Lifecycler,
	claimCh chan<- claimUpdate,
) {

	claim := evt.claim.Claim
	if _, err := getService(claim, catalogLookup); err != nil {
		select {
		case claimCh <- newErrClaimUpdate(claim, err):
		case <-ctx.Done():
		}
		return
	}

	claim.Status = mode.StatusBinding.String()
	select {
	case claimCh <- newClaimUpdate(claim):
	case <-ctx.Done():
		return
	}

	instanceID := claim.InstanceID
	if instanceID == "" {
		select {
		case claimCh <- newErrClaimUpdate(claim, errMissingInstanceID):
		case <-ctx.Done():
		}
		return
	}

	bindID := uuid.New()
	claim.BindID = bindID
	bindRes, err := lifecycler.Bind(instanceID, bindID, &mode.BindRequest{})
	if err != nil {
		select {
		case claimCh <- newErrClaimUpdate(claim, err):
		case <-ctx.Done():
		}
		return
	}

	if _, err := cmNamespacer.ConfigMaps(claim.TargetNamespace).Create(&api.ConfigMap{
		TypeMeta: unversioned.TypeMeta{},
		ObjectMeta: api.ObjectMeta{
			Name:      claim.TargetName,
			Namespace: claim.TargetNamespace,
		},
		Data: bindRes.Creds,
	}); err != nil {
		select {
		case claimCh <- newErrClaimUpdate(claim, err):
		case <-ctx.Done():
		}
		return
	}
	claim.Status = mode.StatusBound.String()
	select {
	case claimCh <- newClaimUpdate(claim):
	case <-ctx.Done():
		return
	}
}

func processUnbind(
	ctx context.Context,
	evt *Event,
	cmNamespacer kcl.ConfigMapsNamespacer,
	catalogLookup k8s.ServiceCatalogLookup,
	lifecycler mode.Lifecycler,
	claimCh chan<- claimUpdate,
) {

	claim := evt.claim.Claim
	if _, err := getService(claim, catalogLookup); err != nil {
		select {
		case claimCh <- newErrClaimUpdate(claim, err):
		case <-ctx.Done():
		}
		return
	}

	claim.Status = mode.StatusUnbinding.String()
	select {
	case claimCh <- newClaimUpdate(claim):
	case <-ctx.Done():
		return
	}

	instanceID := claim.InstanceID
	bindID := claim.BindID
	if instanceID == "" {
		select {
		case claimCh <- newErrClaimUpdate(claim, errMissingInstanceID):
		case <-ctx.Done():
			return
		}
	}
	if bindID == "" {
		select {
		case claimCh <- newErrClaimUpdate(claim, errMissingBindID):
		case <-ctx.Done():
			return
		}
	}

	if err := lifecycler.Unbind(claim.ServiceID, claim.PlanID, instanceID, bindID); err != nil {
		select {
		case claimCh <- newErrClaimUpdate(claim, err):
		case <-ctx.Done():
		}
		return
	}

	// delete configmap
	if err := cmNamespacer.ConfigMaps(claim.TargetNamespace).Delete(claim.TargetName); err != nil {
		select {
		case claimCh <- newErrClaimUpdate(claim, err):
		case <-ctx.Done():
		}
		return
	}

	claim.Status = mode.StatusUnbound.String()
	select {
	case claimCh <- newClaimUpdate(claim):
	case <-ctx.Done():
		return
	}
}

func processDeprovision(
	ctx context.Context,
	evt *Event,
	cmNamespacer kcl.ConfigMapsNamespacer,
	catalogLookup k8s.ServiceCatalogLookup,
	lifecycler mode.Lifecycler,
	claimCh chan<- claimUpdate,
) {

	claim := evt.claim.Claim
	if _, err := getService(claim, catalogLookup); err != nil {
		select {
		case claimCh <- newErrClaimUpdate(claim, err):
		case <-ctx.Done():
		}
		return
	}

	claim.Status = mode.StatusDeprovisioning.String()
	select {
	case claimCh <- newClaimUpdate(claim):
	case <-ctx.Done():
		return
	}

	instanceID := claim.InstanceID
	if instanceID == "" {
		select {
		case claimCh <- newErrClaimUpdate(claim, errMissingInstanceID):
		case <-ctx.Done():
		}
		return
	}

	// deprovision
	if _, err := lifecycler.Deprovision(instanceID, claim.ServiceID, claim.PlanID); err != nil {
		select {
		case claimCh <- newErrClaimUpdate(claim, err):
		case <-ctx.Done():
		}
		return
	}
	claim.Status = mode.StatusDeprovisioned.String()
	select {
	case claimCh <- newClaimUpdate(claim):
	case <-ctx.Done():
		return
	}
}
