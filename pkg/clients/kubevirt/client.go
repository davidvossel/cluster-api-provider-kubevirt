/*
Copyright 2018 The Kubernetes Authors.

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

package kubevirt

import (
	"fmt"

	kubernetesclient "github.com/kubevirt/cluster-api-provider-kubevirt/pkg/clients/kubernetes"
	machineapiapierrors "github.com/openshift/machine-api-operator/pkg/controller/machine"
	corev1 "k8s.io/api/core/v1"
	apimachineryerrors "k8s.io/apimachinery/pkg/api/errors"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/kubecli"
)

//go:generate mockgen -source=./client.go -destination=./mock/client_generated.go -package=mock

const (
	// underKubeConfig is secret key containing kubeconfig content of the UnderKube
	underKubeConfig   = "kubeconfig"
	servicePrefixName = "worker-"
)

// ClientBuilderFuncType is function type for building kubevirt client
type ClientBuilderFuncType func(overKubernetesClient kubernetesclient.Client, secretName, namespace string) (Client, error)

// Client is a wrapper object for actual kubevirt clients: virtctl and the kubecli
type Client interface {
	CreateVirtualMachine(namespace string, newVM *v1.VirtualMachine) (*v1.VirtualMachine, error)
	DeleteVirtualMachine(namespace string, name string, options *k8smetav1.DeleteOptions) error
	GetVirtualMachine(namespace string, name string, options *k8smetav1.GetOptions) (*v1.VirtualMachine, error)
	GetVirtualMachineInstance(namespace string, name string, options *k8smetav1.GetOptions) (*v1.VirtualMachineInstance, error)
	ListVirtualMachine(namespace string, options *k8smetav1.ListOptions) (*v1.VirtualMachineList, error)
	UpdateVirtualMachine(namespace string, vm *v1.VirtualMachine) (*v1.VirtualMachine, error)
	PatchVirtualMachine(namespace string, name string, pt types.PatchType, data []byte, subresources ...string) (result *v1.VirtualMachine, err error)
	RestartVirtualMachine(namespace string, name string) error
	StartVirtualMachine(namespace string, name string) error
	StopVirtualMachine(namespace string, name string) error
	CreateService(vmName string, namespace string) (*corev1.Service, error)
	DeleteService(vmName string, namespace string, options *k8smetav1.DeleteOptions) error
}

type client struct {
	kubevirtClient   kubecli.KubevirtClient
	kuberentesClient *kubernetes.Clientset
}

// New creates our client wrapper object for the actual KubeVirt and VirtCtl clients we use.
func New(overKubernetesClient kubernetesclient.Client, secretName, namespace string) (Client, error) {
	if secretName == "" {
		return nil, machineapiapierrors.InvalidMachineConfiguration("KubeVirt credentials secret - Invalid empty secretName")
	}

	if namespace == "" {
		return nil, machineapiapierrors.InvalidMachineConfiguration("KubeVirt credentials secret - Invalid empty namespace")
	}

	userDataSecret, getSecretErr := overKubernetesClient.UserDataSecret(secretName, namespace)
	if getSecretErr != nil {
		if apimachineryerrors.IsNotFound(getSecretErr) {
			return nil, machineapiapierrors.InvalidMachineConfiguration("KubeVirt credentials secret %s/%s: %v not found", namespace, secretName, getSecretErr)
		}
		return nil, getSecretErr
	}
	underKubeConfig, ok := userDataSecret.Data[underKubeConfig]
	if !ok {
		return nil, machineapiapierrors.InvalidMachineConfiguration("KubeVirt credentials secret %v did not contain key %v",
			secretName, underKubeConfig)
	}

	kubevirtConfig, err := clientcmd.NewClientConfigFromBytes(underKubeConfig)
	if err != nil {
		return nil, err
	}
	kubevirtClient, getClientErr := kubecli.GetKubevirtClientFromClientConfig(kubevirtConfig)
	if getClientErr != nil {
		return nil, getClientErr
	}

	kubernetesConfig, err := clientcmd.BuildConfigFromFlags("", string(underKubeConfig))
	if err != nil {
		return nil, err
	}
	kubernetesClient, err := kubernetes.NewForConfig(kubernetesConfig)
	if err != nil {
		return nil, err
	}
	return &client{
		kubevirtClient:   kubevirtClient,
		kuberentesClient: kubernetesClient,
	}, nil
}

func (c *client) CreateVirtualMachine(namespace string, newVM *v1.VirtualMachine) (*v1.VirtualMachine, error) {
	return c.kubevirtClient.VirtualMachine(namespace).Create(newVM)
}

func (c *client) DeleteVirtualMachine(namespace string, name string, options *k8smetav1.DeleteOptions) error {
	return c.kubevirtClient.VirtualMachine(namespace).Delete(name, options)
}

func (c *client) GetVirtualMachine(namespace string, name string, options *k8smetav1.GetOptions) (*v1.VirtualMachine, error) {
	return c.kubevirtClient.VirtualMachine(namespace).Get(name, options)
}

func (c *client) GetVirtualMachineInstance(namespace string, name string, options *k8smetav1.GetOptions) (*v1.VirtualMachineInstance, error) {
	return c.kubevirtClient.VirtualMachineInstance(namespace).Get(name, options)
}

func (c *client) ListVirtualMachine(namespace string, options *k8smetav1.ListOptions) (*v1.VirtualMachineList, error) {
	return c.kubevirtClient.VirtualMachine(namespace).List(options)
}

func (c *client) UpdateVirtualMachine(namespace string, vm *v1.VirtualMachine) (*v1.VirtualMachine, error) {
	return c.kubevirtClient.VirtualMachine(namespace).Update(vm)
}

func (c *client) PatchVirtualMachine(namespace string, name string, pt types.PatchType, data []byte, subresources ...string) (result *v1.VirtualMachine, err error) {
	return c.kubevirtClient.VirtualMachine(namespace).Patch(name, pt, data, subresources...)
}

func (c *client) RestartVirtualMachine(namespace string, name string) error {
	return c.kubevirtClient.VirtualMachine(namespace).Restart(name)
}

func (c *client) StartVirtualMachine(namespace string, name string) error {
	return c.kubevirtClient.VirtualMachine(namespace).Start(name)
}

func (c *client) StopVirtualMachine(namespace string, name string) error {
	return c.kubevirtClient.VirtualMachine(namespace).Stop(name)
}

func (c *client) CreateService(vmName string, namespace string) (*corev1.Service, error) {
	service := &corev1.Service{}
	service.Name = fmt.Sprint(servicePrefixName, vmName)
	service.Spec = corev1.ServiceSpec{
		ClusterIP: "",
		Selector:  map[string]string{"name": "worker-" + vmName},
	}
	return c.kuberentesClient.CoreV1().Services(namespace).Create(service)
}

func (c *client) DeleteService(vmName string, namespace string, options *k8smetav1.DeleteOptions) error {
	return c.kuberentesClient.CoreV1().Services(namespace).Delete(fmt.Sprint(servicePrefixName, vmName), options)
}
