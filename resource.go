/*
Copyright 2016 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Some of the code was modified from https://github.com/coreos/etcd-operator
which also has the apache 2.0 license.
*/

// Package kit for Kubernetes operators
package operatorkit

import (
	"fmt"
	"time"

	"k8s.io/api/extensions/v1beta1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	errorsUtil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/util/version"
)

// CustomResource is for creating a Kubernetes TPR/CRD
type CustomResource struct {
	// Name of the custom resource
	Name string

	// Plural of the custom resource in plural
	Plural string

	// Group the custom resource belongs to
	Group string

	// Version which should be defined in a const above
	Version string

	// Scope of the CRD. Namespaced or cluster
	Scope apiextensionsv1beta1.ResourceScope

	// Kind is the serialized interface of the resource.
	Kind string
}

// Context hold the clientsets used for creating and watching custom resources
type Context struct {
	Clientset             kubernetes.Interface
	APIExtensionClientset apiextensionsclient.Interface
	Interval              time.Duration
	Timeout               time.Duration
}

// CreateCustomResources creates the given custom resources and waits for them to initialize
// The resource is of kind CRD if the Kubernetes server is 1.7.0 and above.
// The resource is of kind TPR if the Kubernetes server is below 1.7.0.
func CreateCustomResources(context Context, resources []CustomResource) error {

	// CRD is available on v1.7.0 and above. TPR became deprecated on v1.7.0
	serverVersion, err := context.Clientset.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("Error getting server version: %v", err)
	}
	kubeVersion := version.MustParseSemantic(serverVersion.GitVersion)

	var lastErr error
	if kubeVersion.AtLeast(version.MustParseSemantic(serverVersionV170)) {
		for _, resource := range resources {
			err = createCRD(context, resource)
			if err != nil {
				lastErr = err
			}
		}

		for _, resource := range resources {
			if err := waitForCRDInit(context, resource); err != nil {
				lastErr = err
			}
		}
	} else {
		// Create and wait for TPR resources
		for _, resource := range resources {
			err = createTPR(context, resource)
			if err != nil {
				lastErr = err
			}
		}

		for _, resource := range resources {
			if err := waitForTPRInit(context, resource); err != nil {
				lastErr = err
			}
		}
	}
	return lastErr
}

func createCRD(context Context, resource CustomResource) error {
	crdName := fmt.Sprintf("%s.%s", resource.Plural, resource.Group)
	crd := &apiextensionsv1beta1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: crdName,
		},
		Spec: apiextensionsv1beta1.CustomResourceDefinitionSpec{
			Group:   resource.Group,
			Version: resource.Version,
			Scope:   resource.Scope,
			Names: apiextensionsv1beta1.CustomResourceDefinitionNames{
				Singular: resource.Name,
				Plural:   resource.Plural,
				Kind:     resource.Kind,
			},
		},
	}

	_, err := context.APIExtensionClientset.ApiextensionsV1beta1().CustomResourceDefinitions().Create(crd)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create %s CRD. %+v", resource.Name, err)
		}
	}
	return nil
}

func waitForCRDInit(context Context, resource CustomResource) error {
	crdName := fmt.Sprintf("%s.%s", resource.Plural, resource.Group)
	return wait.Poll(context.Interval, context.Timeout, func() (bool, error) {
		crd, err := context.APIExtensionClientset.ApiextensionsV1beta1().CustomResourceDefinitions().Get(crdName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, cond := range crd.Status.Conditions {
			switch cond.Type {
			case apiextensionsv1beta1.Established:
				if cond.Status == apiextensionsv1beta1.ConditionTrue {
					return true, nil
				}
			case apiextensionsv1beta1.NamesAccepted:
				if cond.Status == apiextensionsv1beta1.ConditionFalse {
					return false, fmt.Errorf("Name conflict: %v\n", cond.Reason)
				}
			}
		}
		return false, nil
	})
}

func createTPR(context Context, resource CustomResource) error {
	tprName := fmt.Sprintf("%s.%s", resource.Name, resource.Group)
	tpr := &v1beta1.ThirdPartyResource{
		ObjectMeta: metav1.ObjectMeta{
			Name: tprName,
		},
		Versions: []v1beta1.APIVersion{
			{Name: resource.Version},
		},
		Description: fmt.Sprintf("ThirdPartyResource for %s", resource.Name),
	}
	_, err := context.Clientset.ExtensionsV1beta1().ThirdPartyResources().Create(tpr)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create %s TPR. %+v", resource.Name, err)
		}
	}
	return nil
}

func waitForTPRInit(context Context, resource CustomResource) error {
	// wait for TPR being established
	restcli := context.Clientset.CoreV1().RESTClient()
	uri := fmt.Sprintf("apis/%s/%s/%s", resource.Group, resource.Version, resource.Plural)
	tprName := fmt.Sprintf("%s.%s", resource.Name, resource.Group)

	err := wait.Poll(context.Interval, context.Timeout, func() (bool, error) {
		_, err := restcli.Get().RequestURI(uri).DoRaw()
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil

	})
	if err != nil {
		deleteErr := context.Clientset.ExtensionsV1beta1().ThirdPartyResources().Delete(tprName, nil)
		if deleteErr != nil {
			return errorsUtil.NewAggregate([]error{err, deleteErr})
		}
		return err
	}
	return nil
}
