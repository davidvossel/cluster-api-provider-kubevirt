package vm

import (
	"fmt"
	"strings"
	"time"

	kubevirtclient "github.com/kubevirt/cluster-api-provider-kubevirt/pkg/client"
	machinev1 "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"

	machinecontroller "github.com/openshift/machine-api-operator/pkg/controller/machine"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetesclient "k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	kubevirtapiv1 "kubevirt.io/client-go/api/v1"
)

const (
	requeueAfterSeconds      = 20
	requeueAfterFatalSeconds = 180
	masterLabel              = "node-role.kubernetes.io/master"
)

// ProviderVM runs the logic to reconciles a machine resource towards its desired state
type ProviderVM interface {
	Create(machine *machinev1.Machine) error
	Delete(machine *machinev1.Machine) error
	Update(machine *machinev1.Machine) error
	Exists(machine *machinev1.Machine) (bool, error)
}

// manager is the struct which implement ProviderVM interface
// Use kubernetesClient to access secret params assigned by user
// Use kubevirtClientBuilder to create the OverKube kubevirtClient used
type manager struct {
	kubevirtClientBuilder kubevirtclient.KubevirtClientBuilderFuncType
	// api server controller runtime client
	kubernetesClient *kubernetesclient.Clientset
}

// New creates provider vm instance
func New(kubevirtClientBuilder kubevirtclient.KubevirtClientBuilderFuncType, kubernetesClient *kubernetesclient.Clientset) ProviderVM {
	return &manager{
		kubernetesClient:      kubernetesClient,
		kubevirtClientBuilder: kubevirtClientBuilder,
	}
}

// Create creates machine if it does not exists.
func (m *manager) Create(machine *machinev1.Machine) (err error) {
	machineScope, machineScopeErr := m.prepareMachineScope(machine, "create")
	if machineScopeErr != nil {
		return machineScopeErr
	}

	defer func() {
		// After the operation is done (success or failure)
		// Update the machine object with the relevant changes
		patchhMachineErr := machineScope.patchMachine()
		if patchhMachineErr != nil {
			err = patchhMachineErr
		}
	}()

	vm, createVMErr := m.createVM(machineScope.virtualMachine, machineScope)
	if createVMErr != nil {
		klog.Errorf("%s: error creating machine: %v", machineScope.getMachineName(), createVMErr)
		conditionFailed := conditionFailed()
		conditionFailed.Message = createVMErr.Error()
		machineScope.setProviderStatus(nil, conditionFailed)
		return fmt.Errorf("failed to create virtual machine: %w", createVMErr)
	}

	klog.Infof("Created Machine %v", machineScope.getMachineName())

	machineScope.setProviderID(vm)

	if specificErr := machineScope.setMachineCloudProviderSpecifics(vm); specificErr != nil {
		return fmt.Errorf("failed to set machine cloud provider specifics: %w", specificErr)
	}

	machineScope.setProviderStatus(vm, conditionSuccess())

	return m.requeueIfInstancePending(vm, machineScope.getMachineName())
}

// delete deletes machine
func (m *manager) Delete(machine *machinev1.Machine) (err error) {
	machineScope, machineScopeErr := m.prepareMachineScope(machine, "delete")
	if machineScopeErr != nil {
		return machineScopeErr
	}

	defer func() {
		// After the operation is done (success or failure)
		// Update the machine object with the relevant changes
		patchhMachineErr := machineScope.patchMachine()
		if patchhMachineErr != nil {
			err = patchhMachineErr
		}
	}()

	existingVM, existingVMErr := m.getVM(machineScope.virtualMachine.GetName(), machineScope)
	if existingVMErr != nil {
		// TODO ask Nir how to check it  github.com/jinzhu/gorm  + gorm.IsRecordNotFoundError(existingVMErr)
		if strings.Contains(existingVMErr.Error(), "not found") {
			klog.Infof("%s: VM does not exist", machineScope.getMachineName())
			return nil
		}
		klog.Errorf("%s: error getting existing VM: %v", machineScope.getMachineName(), existingVMErr)
		return existingVMErr
	}

	if existingVM == nil {
		klog.Warningf("%s: VM not found to delete for machine", machineScope.getMachineName())
		return nil
	}

	if deleteVMErr := m.deleteVM(machineScope.virtualMachine.GetName(), machineScope); deleteVMErr != nil {
		return fmt.Errorf("failed to delete VM: %w", deleteVMErr)
	}

	klog.Infof("Deleted machine %v", machineScope.getMachineName())

	return nil
}

// update finds a vm and reconciles the machine resource status against it.
func (m *manager) Update(machine *machinev1.Machine) (err error) {
	machineScope, machineScopeErr := m.prepareMachineScope(machine, "update")
	if machineScopeErr != nil {
		return machineScopeErr
	}

	defer func() {
		// After the operation is done (success or failure)
		// Update the machine object with the relevant changes
		patchhMachineErr := machineScope.patchMachine()
		if patchhMachineErr != nil {
			err = patchhMachineErr
		}
	}()

	existingVM, existingVMErr := m.getVM(machineScope.virtualMachine.GetName(), machineScope)
	if existingVMErr != nil {
		klog.Errorf("%s: error getting existing VM: %v", machineScope.getMachineName(), existingVMErr)
		return existingVMErr
	}

	//TODO Danielle - update ProviderID to lowercase
	if existingVM == nil {
		if machineScope.updateAllowed() {
			klog.Infof("%s: Possible eventual-consistency discrepancy; returning an error to requeue", machineScope.getMachineName())
			return &machinecontroller.RequeueAfterError{RequeueAfter: requeueAfterSeconds * time.Second}
		}
		klog.Warningf("%s: attempted to update machine but the VM found", machineScope.getMachineName())
		//TODO Danielle - understand that case
		// Update status to clear out machine details.
		machineScope.setProviderStatus(nil, conditionSuccess())
		// This is an unrecoverable error condition.  We should delay to
		// minimize unnecessary API calls.
		return &machinecontroller.RequeueAfterError{RequeueAfter: requeueAfterFatalSeconds * time.Second}
	}

	machineScope.virtualMachine.ObjectMeta.ResourceVersion = existingVM.ObjectMeta.ResourceVersion
	updatedVM, updateVMErr := m.updateVM(machineScope.virtualMachine, machineScope)

	if updateVMErr != nil {
		return fmt.Errorf("failed to update VM: %w", updateVMErr)
	}
	machineScope.setProviderID(updatedVM)

	if err := machineScope.setMachineCloudProviderSpecifics(updatedVM); err != nil {
		return fmt.Errorf("failed to set machine cloud provider specifics: %w", err)
	}

	klog.Infof("Updated machine %s", machineScope.getMachineName())
	machineScope.setProviderStatus(updatedVM, conditionSuccess())

	getUpdatedVM, getUpdatedVMErr := m.getVM(machineScope.virtualMachine.GetName(), machineScope)
	if getUpdatedVMErr != nil {
		klog.Errorf("%s: error getting updated VM: %v", machineScope.getMachineName(), existingVMErr)
		getUpdatedVM = updatedVM
	}
	return m.requeueIfInstancePending(getUpdatedVM, machineScope.getMachineName())
}

// exists returns true if machine exists.
func (m *manager) Exists(machine *machinev1.Machine) (bool, error) {
	machineScope, machineScopeErr := m.prepareMachineScope(machine, "check exists")
	if machineScopeErr != nil {
		return false, machineScopeErr
	}

	existingVM, existingVMErr := m.getVM(machineScope.virtualMachine.GetName(), machineScope)
	if existingVMErr != nil {
		// TODO ask Nir how to check it  github.com/jinzhu/gorm  + gorm.IsRecordNotFoundError(existingVMErr)
		if strings.Contains(existingVMErr.Error(), "not found") {
			klog.Infof("%s: VM does not exist", machineScope.getMachineName())
			return false, nil
		}
		klog.Errorf("%s: error getting existing VM: %v", machineScope.getMachineName(), existingVMErr)
		return false, existingVMErr
	}
	if existingVM == nil {
		klog.Infof("%s: VM does not exist", machineScope.getMachineName())
		return false, nil
	}
	return true, existingVMErr
}

func (m *manager) prepareMachineScope(machine *machinev1.Machine, action string) (*machineScope, error) {
	machineScope, machineScopeErr := newMachineScope(machine, m.kubernetesClient, m.kubevirtClientBuilder)
	if machineScopeErr != nil {
		return nil, machineScopeErr
	}

	klog.Infof("%s: %s machine", machineScope.getMachineName(), action)

	return machineScope, nil
}

func (m *manager) createVM(virtualMachine *kubevirtapiv1.VirtualMachine, machineScope *machineScope) (*kubevirtapiv1.VirtualMachine, error) {
	return machineScope.kubevirtClient.CreateVirtualMachine(machineScope.getMachineNamespace(), virtualMachine)
}

func (m *manager) getVM(vmName string, machineScope *machineScope) (*kubevirtapiv1.VirtualMachine, error) {
	return machineScope.kubevirtClient.GetVirtualMachine(machineScope.getMachineNamespace(), vmName, &k8smetav1.GetOptions{})
}

func (m *manager) deleteVM(vmName string, machineScope *machineScope) error {
	gracePeriod := int64(10)
	return machineScope.kubevirtClient.DeleteVirtualMachine(machineScope.getMachineNamespace(), vmName, &k8smetav1.DeleteOptions{GracePeriodSeconds: &gracePeriod})
}

func (m *manager) updateVM(updatedVM *kubevirtapiv1.VirtualMachine, machineScope *machineScope) (*kubevirtapiv1.VirtualMachine, error) {
	return machineScope.kubevirtClient.UpdateVirtualMachine(machineScope.getMachineNamespace(), updatedVM)
}

// isMaster returns true if the machine is part of a cluster's control plane
func (m *manager) isMaster() (bool, error) {
	// TODO implement
	// if p.machine.Status.NodeRef == nil {
	// 	klog.Errorf("NodeRef not found in machine %s", p.machine.Name)
	// 	return false, nil
	// }
	// node := &corev1.Node{}
	// nodeKey := types.NamespacedName{
	// 	Namespace: p.machine.Status.NodeRef.Namespace,
	// 	Name:      p.machine.Status.NodeRef.Name,
	// }

	// err := p.client.Get(p.Context, nodeKey, node)
	// if err != nil {
	// 	return false, fmt.Errorf("failed to get node from machine %s", p.machine.Name)
	// }

	// if _, exists := node.Labels[masterLabel]; exists {
	// 	return true, nil
	// }
	return false, nil
}

// // updateLoadBalancers adds a given machine instance to the load balancers specified in its provider config
// func (r *providerVM) updateLoadBalancers(instance *ec2.Instance) error {
// 	if len(r.providerSpec.LoadBalancers) == 0 {
// 		klog.V(4).Infof("%s: Instance %q has no load balancers configured. Skipping", r.machine.Name, *instance.InstanceId)
// 		return nil
// 	}
// 	errs := []error{}
// 	classicLoadBalancerNames := []string{}
// 	networkLoadBalancerNames := []string{}
// 	for _, loadBalancerRef := range r.providerSpec.LoadBalancers {
// 		switch loadBalancerRef.Type {
// 		case kubevirtproviderv1.NetworkLoadBalancerType:
// 			networkLoadBalancerNames = append(networkLoadBalancerNames, loadBalancerRef.Name)
// 		case kubevirtproviderv1.ClassicLoadBalancerType:
// 			classicLoadBalancerNames = append(classicLoadBalancerNames, loadBalancerRef.Name)
// 		}
// 	}

// 	var err error
// 	if len(classicLoadBalancerNames) > 0 {
// 		err := registerWithClassicLoadBalancers(r.awsClient, classicLoadBalancerNames, instance)
// 		if err != nil {
// 			klog.Errorf("%s: Failed to register classic load balancers: %v", r.machine.Name, err)
// 			errs = append(errs, err)
// 		}
// 	}
// 	if len(networkLoadBalancerNames) > 0 {
// 		err = registerWithNetworkLoadBalancers(r.awsClient, networkLoadBalancerNames, instance)
// 		if err != nil {
// 			klog.Errorf("%s: Failed to register network load balancers: %v", r.machine.Name, err)
// 			errs = append(errs, err)
// 		}
// 	}
// 	if len(errs) > 0 {
// 		return errorutil.NewAggregate(errs)
// 	}
// 	return nil
// }

func (m *manager) requeueIfInstancePending(vm *kubevirtapiv1.VirtualMachine, machineName string) error {
	// If machine state is still pending, we will return an error to keep the controllers
	// attempting to update status until it hits a more permanent state. This will ensure
	// we get a public IP populated more quickly.
	if !vm.Status.Ready {
		klog.Infof("%s: VM status is not ready, returning an error to requeue", machineName)
		return &machinecontroller.RequeueAfterError{RequeueAfter: requeueAfterSeconds * time.Second}
	}

	return nil
}

//
//handlerNodeSelector := fields.ParseSelectorOrDie("spec.nodeName=" + nodeName)
//labelSelector, err := labels.Parse(virtv1.AppLabel + " in (virt-handler)")
//k8smetav1.ListOptions{
//	FieldSelector: handlerNodeSelector.String(),
//	LabelSelector: labelSelector.String()}
//func listVMs(underkubeclient kubevirtclient.Client, namespace string) (*kubevirtapiv1.VirtualMachineList, error) {
//	//filter the vms with a specific tag '{}'
//	//machine.Labels map[string]string
//	labelSelector, _ := labels.Parse(kubevirtapiv1.AppLabel + " in (virt-handler)")
//
//	aa := k8smetav1.ListOptions{
//		LabelSelector: labelSelector.String(),
//	}
//
//	return underkubeclient.ListVirtualMachine(namespace, &aa)
//}

// getRunningFromVms returns all running vms from a list of vms.
func (m *manager) getRunningFromVms(vms []*kubevirtapiv1.VirtualMachine) []*kubevirtapiv1.VirtualMachine {
	var runningVms []*kubevirtapiv1.VirtualMachine
	for _, vm := range vms {
		if vm.Status.Ready {
			runningVms = append(runningVms, vm)
		}
	}
	return runningVms
}

// getStoppedVms returns all stopped vms that have a tag matching our machine name,
// and cluster ID.
func (m *manager) getStoppedVms(machine *machinev1.Machine, client kubevirtclient.Client) ([]*kubevirtapiv1.VirtualMachine, error) {
	// TODO implement
	// stoppedInstanceStateFilter := []*string{aws.String(ec2.InstanceStateNameStopped), aws.String(ec2.InstanceStateNameStopping)}
	// return getInstances(machine, client, stoppedInstanceStateFilter)
	return nil, nil
}

// getExistingVms returns all vms not terminated
func (m *manager) getExistingVms(machine *machinev1.Machine, client kubevirtclient.Client) ([]*kubevirtapiv1.VirtualMachine, error) {
	// TODO implement
	// return getInstances(machine, client, existingInstanceStates())
	return nil, nil
}

func (m *manager) getExistingVMByID(id string, client kubevirtclient.Client) (*kubevirtapiv1.VirtualMachine, error) {
	// TODO implement
	// return getInstanceByID(id, client, existingInstanceStates())
	return nil, nil
}

// func instanceHasAllowedState(instance *ec2.Instance, instanceStateFilter []*string) error {
// 	if instance.InstanceId == nil {
// 		return fmt.Errorf("instance has nil ID")
// 	}

// 	if instance.State == nil {
// 		return fmt.Errorf("instance %s has nil state", *instance.InstanceId)
// 	}

// 	if len(instanceStateFilter) == 0 {
// 		return nil
// 	}

// 	actualState := aws.StringValue(instance.State.Name)
// 	for _, allowedState := range instanceStateFilter {
// 		if aws.StringValue(allowedState) == actualState {
// 			return nil
// 		}
// 	}

// 	allowedStates := make([]string, 0, len(instanceStateFilter))
// 	for _, allowedState := range instanceStateFilter {
// 		allowedStates = append(allowedStates, aws.StringValue(allowedState))
// 	}
// 	return fmt.Errorf("instance %s state %q is not in %s", *instance.InstanceId, actualState, strings.Join(allowedStates, ", "))
// }

// getVmByID returns the vm with the given ID if it exists.
func (m *manager) getVMByID(id string, client kubevirtclient.Client, instanceStateFilter []*string) (*kubevirtapiv1.VirtualMachine, error) {
	// TODO implement
	// if id == "" {
	// 	return nil, fmt.Errorf("instance-id not specified")
	// }

	// request := &ec2.DescribeInstancesInput{
	// 	InstanceIds: aws.StringSlice([]string{id}),
	// }

	// result, err := client.DescribeInstances(request)
	// if err != nil {
	// 	return nil, err
	// }

	// if len(result.Reservations) != 1 {
	// 	return nil, fmt.Errorf("found %d reservations for instance-id %s", len(result.Reservations), id)
	// }

	// reservation := result.Reservations[0]

	// if len(reservation.Instances) != 1 {
	// 	return nil, fmt.Errorf("found %d instances for instance-id %s", len(reservation.Instances), id)
	// }

	// instance := reservation.Instances[0]

	// return instance, instanceHasAllowedState(instance, instanceStateFilter)
	return nil, nil
}

// getVms returns all vms that have a tag matching our machine name,
// and cluster ID.
func (m *manager) getVms(machine *machinev1.Machine, client kubevirtclient.Client, vmStateFilter []*string) ([]*kubevirtapiv1.VirtualMachine, error) {
	// TODO implement
	// clusterID, ok := getClusterID(machine)
	// if !ok {
	// 	return []*ec2.Instance{}, fmt.Errorf("unable to get cluster ID for machine: %q", machine.Name)
	// }

	// requestFilters := []*ec2.Filter{
	// 	{
	// 		Name:   awsTagFilter("Name"),
	// 		Values: aws.StringSlice([]string{machine.Name}),
	// 	},
	// 	clusterFilter(clusterID),
	// }

	// request := &ec2.DescribeInstancesInput{
	// 	Filters: requestFilters,
	// }

	// result, err := client.DescribeInstances(request)
	// if err != nil {
	// 	return []*ec2.Instance{}, err
	// }

	// instances := make([]*ec2.Instance, 0, len(result.Reservations))

	// for _, reservation := range result.Reservations {
	// 	for _, instance := range reservation.Instances {
	// 		err := instanceHasAllowedState(instance, instanceStateFilter)
	// 		if err != nil {
	// 			klog.Errorf("Excluding instance matching %s: %v", machine.Name, err)
	// 		} else {
	// 			instances = append(instances, instance)
	// 		}
	// 	}
	// }

	// return instances, nil
	return nil, nil
}

// terminateVms terminates all provided vms with a single virtctl request.
func (m *manager) terminateVms(client kubevirtclient.Client, vms []*kubevirtapiv1.VirtualMachine) error {
	// TODO implement: should return similar to aws? []*kubevirtapiv1.VirtualMachineState
	// instanceIDs := []*string{}
	// // Cleanup all older instances:
	// for _, instance := range instances {
	// 	klog.Infof("Cleaning up extraneous instance for machine: %v, state: %v, launchTime: %v", *instance.InstanceId, *instance.State.Name, *instance.LaunchTime)
	// 	instanceIDs = append(instanceIDs, instance.InstanceId)
	// }
	// for _, instanceID := range instanceIDs {
	// 	klog.Infof("Terminating %v instance", *instanceID)
	// }

	// terminateInstancesRequest := &ec2.TerminateInstancesInput{
	// 	InstanceIds: instanceIDs,
	// }
	// output, err := client.TerminateInstances(terminateInstancesRequest)
	// if err != nil {
	// 	klog.Errorf("Error terminating instances: %v", err)
	// 	return nil, fmt.Errorf("error terminating instances: %v", err)
	// }

	// if output == nil {
	// 	return nil, nil
	// }

	// return output.TerminatingInstances, nil
	return nil
}
