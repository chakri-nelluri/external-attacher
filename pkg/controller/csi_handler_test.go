/*
Copyright 2017 The Kubernetes Authors.

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

package controller

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kubernetes-csi/external-attacher-csi/pkg/connection"

	"k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	core "k8s.io/client-go/testing"
)

func csiHandlerFactory(client kubernetes.Interface, informerFactory informers.SharedInformerFactory, csi connection.CSIConnection) Handler {
	return NewCSIHandler(
		client,
		testAttacherName,
		csi,
		informerFactory.Core().V1().PersistentVolumes().Lister(),
		informerFactory.Core().V1().Nodes().Lister(),
		informerFactory.Storage().V1().VolumeAttachments().Lister())
}

func pv() *v1.PersistentVolume {
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: testPVName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       testAttacherName,
					VolumeHandle: testVolumeHandle,
					ReadOnly:     false,
				},
			},
		},
	}
}

func pvWithFinalizer() *v1.PersistentVolume {
	pv := pv()
	pv.Finalizers = []string{"attacher-csi/test"}
	return pv
}

func pvWithFinalizers(pv *v1.PersistentVolume, finalizers ...string) *v1.PersistentVolume {
	pv.Finalizers = append(pv.Finalizers, finalizers...)
	return pv
}

func pvDeleted(pv *v1.PersistentVolume) *v1.PersistentVolume {
	pv.DeletionTimestamp = &metav1.Time{}
	return pv
}

func node() *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testNodeName,
			Annotations: map[string]string{"nodeid.csi.volume.kubernetes.io/foo_bar": "MyNodeID"},
		},
	}
}

func TestCSIHandler(t *testing.T) {
	vaGroupResourceVersion := schema.GroupVersionResource{
		Group:    storagev1.GroupName,
		Version:  "v1",
		Resource: "volumeattachments",
	}
	pvGroupResourceVersion := schema.GroupVersionResource{
		Group:    v1.GroupName,
		Version:  "v1",
		Resource: "persistentvolumes",
	}

	tests := []testCase{
		//
		// ATTACH
		//
		{
			name:           "VolumeAttachment added -> successful attachment",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			addedVA:        va(false /*attached*/, "" /*finalizer*/),
			expectedActions: []core.Action{
				// Finalizer is saved first
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(false /*attached*/, "attacher-csi/test")),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(true /*attached*/, "attacher-csi/test")),
			},
			expectedCSICalls: []csiCall{
				{"attach", testPVName, testNodeName, nil, nil},
			},
		},
		{
			name:           "VolumeAttachment updated -> successful attachment",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			updatedVA:      va(false, ""),
			expectedActions: []core.Action{
				// Finalizer is saved first
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(false /*attached*/, "attacher-csi/test")),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(true /*attached*/, "attacher-csi/test")),
			},
			expectedCSICalls: []csiCall{
				{"attach", testPVName, testNodeName, nil, nil},
			},
		},
		{
			name:           "VolumeAttachment updated -> PV finalizer is added",
			initialObjects: []runtime.Object{pv(), node()},
			updatedVA:      va(false, ""),
			expectedActions: []core.Action{
				// VA Finalizer is saved first
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(false /*attached*/, "attacher-csi/test")),
				// PV Finalizer after VA
				core.NewUpdateAction(pvGroupResourceVersion, metav1.NamespaceNone, pvWithFinalizer()),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(true /*attached*/, "attacher-csi/test")),
			},
			expectedCSICalls: []csiCall{
				{"attach", testPVName, testNodeName, nil, nil},
			},
		},
		{
			name:           "error saving PV finalizer -> controller retries",
			initialObjects: []runtime.Object{pv(), node()},
			updatedVA:      va(false, ""),
			reactors: []reaction{
				{
					verb:     "update",
					resource: "persistentvolumes",
					reactor: func(t *testing.T) core.ReactionFunc {
						i := 0
						return func(core.Action) (bool, runtime.Object, error) {
							i++
							if i < 2 {
								// Update fails once
								return true, nil, apierrors.NewForbidden(v1.Resource("persistentvolume"), "pv1", errors.New("Mock error"))
							}
							// Update succeeds for the 2nd time
							return false, nil, nil
						}
					},
				},
			},
			expectedActions: []core.Action{
				// VA Finalizer is saved first
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(false /*attached*/, "attacher-csi/test")),
				// PV Finalizer after VA - fails
				core.NewUpdateAction(pvGroupResourceVersion, metav1.NamespaceNone, pvWithFinalizer()),
				// Error is saved
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithAttachError(va(false, "attacher-csi/test"), "could not add PersistentVolume finalizer: persistentvolume \"pv1\" is forbidden: Mock error")),
				// Second PV Finalizer - succeeds
				core.NewUpdateAction(pvGroupResourceVersion, metav1.NamespaceNone, pvWithFinalizer()),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(true /*attached*/, "attacher-csi/test")),
			},
			expectedCSICalls: []csiCall{
				{"attach", testPVName, testNodeName, nil, nil},
			},
		},
		{
			name:             "already attached volume -> ignored",
			initialObjects:   []runtime.Object{pvWithFinalizer(), node()},
			updatedVA:        va(true, "attacher-csi/test"),
			expectedActions:  []core.Action{},
			expectedCSICalls: []csiCall{},
		},
		{
			name:           "PV with deletion timestamp -> ignored with error",
			initialObjects: []runtime.Object{pvDeleted(pv()), node()},
			updatedVA:      va(false, "attacher-csi/test"),
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithAttachError(va(false, "attacher-csi/test"), "PersistentVolume \"pv1\" is marked for deletion")),
			},
			expectedCSICalls: []csiCall{},
		},
		{
			name:           "VolumeAttachment added -> successful attachment incl. metadata",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			addedVA:        va(false, ""),
			expectedActions: []core.Action{
				// Finalizer is saved first
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(false /*attached*/, "attacher-csi/test")),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithMetadata(va(true, "attacher-csi/test"), map[string]string{"foo": "bar"})),
			},
			expectedCSICalls: []csiCall{
				{"attach", testPVName, testNodeName, nil, map[string]string{"foo": "bar"}},
			},
		},
		{
			name:            "unknown driver -> ignored",
			initialObjects:  []runtime.Object{pvWithFinalizer(), node()},
			addedVA:         vaWithInvalidDriver(va(false, "attacher-csi/test")),
			expectedActions: []core.Action{},
		},
		{
			name:           "unknown PV -> error",
			initialObjects: []runtime.Object{node()},
			addedVA:        va(false, "attacher-csi/test"),
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithAttachError(va(false, "attacher-csi/test"), "persistentvolume \"pv1\" not found")),
			},
		},
		{
			name:           "unknown PV -> error + error saving the error",
			initialObjects: []runtime.Object{node()},
			addedVA:        va(false, "attacher-csi/test"),
			reactors: []reaction{
				{
					verb:     "update",
					resource: "volumeattachments",
					reactor: func(t *testing.T) core.ReactionFunc {
						i := 0
						return func(core.Action) (bool, runtime.Object, error) {
							i++
							if i < 3 {
								// Update fails 2 times
								return true, nil, apierrors.NewForbidden(storagev1.Resource("volumeattachments"), "pv1-node1", errors.New("Mock error"))
							}
							// Update succeeds for the 3rd time
							return false, nil, nil
						}
					},
				},
			},
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithAttachError(va(false, "attacher-csi/test"), "persistentvolume \"pv1\" not found")),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithAttachError(va(false, "attacher-csi/test"), "persistentvolume \"pv1\" not found")),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithAttachError(va(false, "attacher-csi/test"), "persistentvolume \"pv1\" not found")),
			},
		},
		{
			name:           "invalid PV reference-> error",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			addedVA:        vaWithNoPVReference(va(false, "attacher-csi/test")),
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithAttachError(vaWithNoPVReference(va(false, "attacher-csi/test")), "VolumeAttachment.spec.persistentVolumeName is empty")),
			},
		},
		{
			name:           "unknown node -> error",
			initialObjects: []runtime.Object{pvWithFinalizer()},
			addedVA:        va(false, "attacher-csi/test"),
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithAttachError(va(false, "attacher-csi/test"), "node \"node1\" not found")),
			},
		},
		{
			name:           "failed write with VA finializers -> controller retries",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			addedVA:        va(false, ""),
			reactors: []reaction{
				{
					verb:     "update",
					resource: "volumeattachments",
					reactor: func(t *testing.T) core.ReactionFunc {
						i := 0
						return func(core.Action) (bool, runtime.Object, error) {
							i++
							if i < 3 {
								// Update fails 2 times
								return true, nil, apierrors.NewForbidden(storagev1.Resource("volumeattachments"), "pv1-node1", errors.New("Mock error"))
							}
							// Update succeeds for the 3rd time
							return false, nil, nil
						}
					},
				},
			},
			expectedActions: []core.Action{
				// Controller tries to save VA finalizer, it fails
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(false /*attached*/, "attacher-csi/test")),
				// Controller tries to save error, it fails too
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithAttachError(va(false /*attached*/, ""), "could not add VolumeAttachment finalizer: volumeattachments.storage.k8s.io \"pv1-node1\" is forbidden: Mock error")),
				// Controller tries to save VA finalizer again, it succeeds
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(false /*attached*/, "attacher-csi/test")),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(true /*attached*/, "attacher-csi/test")),
			},
			expectedCSICalls: []csiCall{
				{"attach", testPVName, testNodeName, nil, nil},
			},
		},
		{
			name:           "failed write with attached=true -> controller retries",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			addedVA:        va(false, ""),
			reactors: []reaction{
				{
					verb:     "update",
					resource: "volumeattachments",
					reactor: func(t *testing.T) core.ReactionFunc {
						i := 0
						return func(core.Action) (bool, runtime.Object, error) {
							i++
							if i != 2 {
								return false, nil, nil
							}
							return true, nil, apierrors.NewForbidden(storagev1.Resource("volumeattachments"), "pv1-node1", errors.New("mock error"))
						}
					},
				},
			},
			expectedActions: []core.Action{
				// Finalizer is saved first
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(false /*attached*/, "attacher-csi/test")),
				// Second save with attached=true fails
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(true /*attached*/, "attacher-csi/test")),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(true /*attached*/, "attacher-csi/test")),
			},
			expectedCSICalls: []csiCall{
				{"attach", testPVName, testNodeName, nil, nil},
				{"attach", testPVName, testNodeName, nil, nil},
			},
		},
		{
			name:           "CSI attach fails -> controller retries",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			addedVA:        va(false, ""),
			expectedActions: []core.Action{
				// Finalizer is saved first
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(false /*attached*/, "attacher-csi/test")),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithAttachError(va(false, "attacher-csi/test"), "mock error")),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, va(true /*attached*/, "attacher-csi/test")),
			},
			expectedCSICalls: []csiCall{
				{"attach", testPVName, testNodeName, fmt.Errorf("mock error"), nil},
				{"attach", testPVName, testNodeName, nil, nil},
			},
		},
		//
		// DETACH
		//
		{
			name:           "VolumeAttachment marked for deletion -> successful detach",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			addedVA:        deleted(va(true, "attacher-csi/test")),
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, deleted(va(false /*attached*/, ""))),
			},
			expectedCSICalls: []csiCall{
				{"detach", testPVName, testNodeName, nil, nil},
			},
		},
		{
			name:           "CSI detach fails -> controller retries",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			addedVA:        deleted(va(true, "attacher-csi/test")),
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithDetachError(deleted(va(true /*attached*/, "attacher-csi/test")), "mock error")),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, deleted(va(false /*attached*/, ""))),
			},
			expectedCSICalls: []csiCall{
				{"detach", testPVName, testNodeName, fmt.Errorf("mock error"), nil},
				{"detach", testPVName, testNodeName, nil, nil},
			},
		},
		{
			name:             "already detached volume -> ignored",
			initialObjects:   []runtime.Object{pvWithFinalizer(), node()},
			updatedVA:        deleted(va(false, "")),
			expectedActions:  []core.Action{},
			expectedCSICalls: []csiCall{},
		},
		{
			name:           "detach unknown PV -> error",
			initialObjects: []runtime.Object{node()},
			addedVA:        deleted(va(true, "attacher-csi/test")),
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, deleted(vaWithDetachError(va(true, "attacher-csi/test"), "persistentvolume \"pv1\" not found"))),
			},
		},
		{
			name:           "detach unknown PV -> error + error saving the error",
			initialObjects: []runtime.Object{node()},
			addedVA:        deleted(va(true, "attacher-csi/test")),
			reactors: []reaction{
				{
					verb:     "update",
					resource: "volumeattachments",
					reactor: func(t *testing.T) core.ReactionFunc {
						i := 0
						return func(core.Action) (bool, runtime.Object, error) {
							i++
							if i < 3 {
								// Update fails 2 times
								return true, nil, apierrors.NewForbidden(storagev1.Resource("volumeattachments"), "pv1-node1", errors.New("Mock error"))
							}
							// Update succeeds for the 3rd time
							return false, nil, nil
						}
					},
				},
			},
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, deleted(vaWithDetachError(va(true, "attacher-csi/test"), "persistentvolume \"pv1\" not found"))),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, deleted(vaWithDetachError(va(true, "attacher-csi/test"), "persistentvolume \"pv1\" not found"))),
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, deleted(vaWithDetachError(va(true, "attacher-csi/test"), "persistentvolume \"pv1\" not found"))),
			},
		},
		{
			name:           "detach invalid PV reference-> error",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			addedVA:        deleted(vaWithNoPVReference(va(true, "attacher-csi/test"))),
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, deleted(vaWithDetachError(vaWithNoPVReference(va(true, "attacher-csi/test")), "VolumeAttachment.spec.persistentVolumeName is empty"))),
			},
		},
		{
			name:           "detach unknown node -> error",
			initialObjects: []runtime.Object{pvWithFinalizer()},
			addedVA:        deleted(va(true, "attacher-csi/test")),
			expectedActions: []core.Action{
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, deleted(vaWithDetachError(va(true, "attacher-csi/test"), "node \"node1\" not found"))),
			},
		},
		{
			name:           "failed write with attached=false -> controller retries",
			initialObjects: []runtime.Object{pvWithFinalizer(), node()},
			addedVA:        deleted(va(false, "attacher-csi/test")),
			reactors: []reaction{
				{
					verb:     "update",
					resource: "volumeattachments",
					reactor: func(t *testing.T) core.ReactionFunc {
						i := 0
						return func(core.Action) (bool, runtime.Object, error) {
							i++
							if i == 1 {
								return true, nil, apierrors.NewForbidden(storagev1.Resource("volumeattachments"), "pv1-node1", errors.New("mock error"))
							}
							return false, nil, nil
						}
					},
				},
			},
			expectedActions: []core.Action{
				// This fails
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, deleted(va(false, ""))),
				// Saving error succeeds
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, vaWithDetachError(deleted(va(false, "attacher-csi/test")), "could not mark as detached: volumeattachments.storage.k8s.io \"pv1-node1\" is forbidden: mock error")),
				// Second save of attached=false succeeds
				core.NewUpdateAction(vaGroupResourceVersion, metav1.NamespaceNone, deleted(va(false, ""))),
			},
			expectedCSICalls: []csiCall{
				{"detach", testPVName, testNodeName, nil, nil},
				{"detach", testPVName, testNodeName, nil, nil},
			},
		},

		//
		// PV finalizers
		//
		{
			name:           "VA deleted -> PV finalizer removed",
			initialObjects: []runtime.Object{pvDeleted(pvWithFinalizer())},
			deletedVA:      va(false, ""),
			expectedActions: []core.Action{
				core.NewUpdateAction(pvGroupResourceVersion, metav1.NamespaceNone, pvDeleted(pv())),
			},
		},
		{
			name:           "PV updated -> PV finalizer removed",
			initialObjects: []runtime.Object{},
			updatedPV:      pvDeleted(pvWithFinalizer()),
			expectedActions: []core.Action{
				core.NewUpdateAction(pvGroupResourceVersion, metav1.NamespaceNone, pvDeleted(pv())),
			},
		},
		{
			name:           "PV finalizer removed -> other finalizers preserved",
			initialObjects: []runtime.Object{pvDeleted(pvWithFinalizers(pvWithFinalizer(), "foo/bar", "bar/baz"))},
			deletedVA:      va(false, ""),
			expectedActions: []core.Action{
				core.NewUpdateAction(pvGroupResourceVersion, metav1.NamespaceNone, pvDeleted(pvWithFinalizers(pv(), "foo/bar", "bar/baz"))),
			},
		},
		{
			name:           "finalizer removal fails -> controller retries",
			initialObjects: []runtime.Object{pvDeleted(pvWithFinalizer())},
			deletedVA:      va(false, ""),
			reactors: []reaction{
				{
					verb:     "update",
					resource: "persistentvolumes",
					reactor: func(t *testing.T) core.ReactionFunc {
						i := 0
						return func(core.Action) (bool, runtime.Object, error) {
							i++
							if i < 3 {
								return true, nil, apierrors.NewForbidden(v1.Resource("persistentvolumes"), "pv1", errors.New("mock error"))
							}
							return false, nil, nil
						}
					},
				},
			},
			expectedActions: []core.Action{
				// This update fails
				core.NewUpdateAction(pvGroupResourceVersion, metav1.NamespaceNone, pvDeleted(pv())),
				// This one fails too
				core.NewUpdateAction(pvGroupResourceVersion, metav1.NamespaceNone, pvDeleted(pv())),
				// This one succeeds
				core.NewUpdateAction(pvGroupResourceVersion, metav1.NamespaceNone, pvDeleted(pv())),
			},
		},
		{
			name:            "no PV finalizer -> ignored",
			initialObjects:  []runtime.Object{pvDeleted(pv())},
			deletedVA:       va(false, ""),
			expectedActions: []core.Action{},
		},
		{
			name:            "no deletion timestamp -> ignored",
			initialObjects:  []runtime.Object{pv()},
			deletedVA:       va(false, ""),
			expectedActions: []core.Action{},
		},
		{
			name:            "VA exists -> ignored",
			initialObjects:  []runtime.Object{pvDeleted(pvWithFinalizer()), va(false, "")},
			deletedVA:       va(false, ""),
			expectedActions: []core.Action{},
		},
	}

	runTests(t, csiHandlerFactory, tests)
}
