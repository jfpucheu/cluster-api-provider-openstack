/*
Copyright 2025 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	. "github.com/onsi/gomega" //nolint:revive
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	capiannotations "sigs.k8s.io/cluster-api/util/annotations"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-openstack/api/v1beta1"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/scope"
)

var (
	flavorID = "661c21bc-be52-44e3-9d2e-8d1e11623b59"
	imageID  = "ce96e584-7ebc-46d6-9e55-987d72e3806c"
)

func TestOpenStackMachineTemplateReconciler_Reconcile_UnhappyPaths(t *testing.T) {
	ctx := context.Background()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = infrav1.AddToScheme(scheme)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns"}}

	tests := []struct {
		name    string
		reqName string
		objects []client.Object
		setup   func(r *OpenStackMachineTemplateReconciler)
		wantErr string
	}{
		{
			name:    "object does not exist",
			reqName: "does-not-exist",
			objects: []client.Object{ns},
		},
		{
			name:    "marked for deletion",
			reqName: "to-be-deleted",
			objects: func() []client.Object {
				tpl := newOSMT("to-be-deleted", "c1", false, false, true)
				now := metav1.Now()
				tpl.DeletionTimestamp = &now
				tpl.Finalizers = []string{"test.finalizer.cluster.x-k8s.io"}
				return []client.Object{ns, tpl}
			}(),
		},
		{
			name:    "missing cluster owner",
			reqName: "no-cluster",
			objects: []client.Object{
				ns,
				func() *infrav1.OpenStackMachineTemplate {
					tpl := newOSMT("no-cluster", "c1", false, false, false)
					delete(tpl.Labels, clusterv1.ClusterNameLabel)
					return tpl
				}(),
			},
		},
		{
			name:    "paused cluster",
			reqName: "paused-cluster",
			objects: []client.Object{
				ns,
				newOSMT("paused-cluster", "paused-cluster", false, false, true),
				newCluster("paused-cluster", "oscluster", true),
				newOSCluster("oscluster"),
			},
		},
		{
			name:    "paused tpl",
			reqName: "paused-tpl",
			objects: []client.Object{
				ns,
				newOSMT("paused-tpl", "cluster", true, false, true),
				newCluster("cluster", "oscluster", false),
				newOSCluster("oscluster"),
			},
		},
		{
			name:    "scope factory returns error",
			reqName: "scope-error",
			objects: []client.Object{
				ns,
				newOSMT("scope-error", "c1", false, false, true),
				newCluster("c1", "oscluster", false),
				newOSCluster("oscluster"),
			},
			setup: func(r *OpenStackMachineTemplateReconciler) {
				mockCtrl := gomock.NewController(t)
				mockScopeFactory := scope.NewMockScopeFactory(mockCtrl, "proj")
				mockScopeFactory.SetClientScopeCreateError(fmt.Errorf("boom"))
				r.ScopeFactory = mockScopeFactory
				t.Cleanup(mockCtrl.Finish)
			},
			wantErr: "boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.objects...).
				Build()

			r := &OpenStackMachineTemplateReconciler{
				Client:       cl,
				ScopeFactory: nil, // set in setup when needed
			}

			if tt.setup != nil {
				tt.setup(r)
			}

			req := ctrl.Request{
				NamespacedName: client.ObjectKey{
					Namespace: "test-ns",
					Name:      tt.reqName,
				},
			}

			_, err := r.Reconcile(ctx, req)
			if tt.wantErr == "" {
				g.Expect(err).ToNot(HaveOccurred())
			} else {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tt.wantErr))
			}
		})
	}
}

func TestOpenStackMachineTemplateReconciler_reconcileNormal(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// --- Scheme setup ---
	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(infrav1.AddToScheme(scheme)).To(Succeed())

	type testCase struct {
		name    string
		tpl     *infrav1.OpenStackMachineTemplate
		expect  func(mf *scope.MockScopeFactory)
		wantErr string
		verify  func(g Gomega, tpl *infrav1.OpenStackMachineTemplate)
	}

	tests := []testCase{
		{
			name: "error getting flavor details",
			tpl:  newOSMT("test-osmt", "test-cluster", false, false, true),
			expect: func(mf *scope.MockScopeFactory) {
				mf.ComputeClient.
					EXPECT().
					GetFlavor(flavorID).
					Return(nil, fmt.Errorf("flavor-details-error"))
			},
			wantErr: "flavor-details-error",
		},
		{
			name: "error getting image details",
			tpl:  newOSMT("test-osmt", "test-cluster", false, false, true),
			expect: func(mf *scope.MockScopeFactory) {
				mf.ComputeClient.
					EXPECT().
					GetFlavor(flavorID).
					Return(&flavors.Flavor{
						VCPUs: 2, RAM: 1024, Disk: 5, Ephemeral: 1,
					}, nil)

				mf.ImageClient.
					EXPECT().
					GetImage(imageID).
					Return(nil, fmt.Errorf("image-details-error"))
			},
			wantErr: "image-details-error",
		},
		{
			name: "boot-from-image",
			tpl:  newOSMT("test-osmt", "test-cluster", false, false, true),
			expect: func(mf *scope.MockScopeFactory) {
				mf.ComputeClient.
					EXPECT().
					GetFlavor(flavorID).
					Return(&flavors.Flavor{
						VCPUs:     4,
						RAM:       8192,
						Disk:      50,
						Ephemeral: 10,
					}, nil)

				mf.ImageClient.
					EXPECT().
					GetImage(imageID).
					Return(&images.Image{
						ID: imageID,
						Properties: map[string]any{
							imagePropertyForOS: "linux",
						},
					}, nil)
			},
			wantErr: "",
			verify: func(g Gomega, tpl *infrav1.OpenStackMachineTemplate) {
				// CPU = 4 cores
				expCPU := *resource.NewQuantity(4, resource.DecimalSI)
				g.Expect(tpl.Status.Capacity[corev1.ResourceCPU]).To(Equal(expCPU))

				// Memory = 8192 MiB → bytes
				ramBytes := int64(8192) * 1024 * 1024
				expMem := *resource.NewQuantity(ramBytes, resource.BinarySI)
				g.Expect(tpl.Status.Capacity[corev1.ResourceMemory]).To(Equal(expMem))

				// Ephemeral = 10 GiB → bytes
				ephBytes := int64(10) * 1024 * 1024 * 1024
				expEph := *resource.NewQuantity(ephBytes, resource.BinarySI)
				g.Expect(tpl.Status.Capacity[corev1.ResourceEphemeralStorage]).To(Equal(expEph))

				// Storage = Disk = 50 GiB → bytes (because RootVolume is nil)
				storageBytes := int64(50) * 1024 * 1024 * 1024
				expStorage := *resource.NewQuantity(storageBytes, resource.BinarySI)
				g.Expect(tpl.Status.Capacity[corev1.ResourceStorage]).To(Equal(expStorage))

				// OS property
				g.Expect(tpl.Status.NodeInfo.OperatingSystem).To(Equal("linux"))
			},
		},
		{
			name: "boot-from-volume",
			tpl:  newOSMT("test-osmt", "test-cluster", false, true, true),
			expect: func(mf *scope.MockScopeFactory) {
				mf.ComputeClient.
					EXPECT().
					GetFlavor(flavorID).
					Return(&flavors.Flavor{
						VCPUs:     4,
						RAM:       8192,
						Disk:      50,
						Ephemeral: 10,
					}, nil)

				mf.ImageClient.
					EXPECT().
					GetImage(imageID).
					Return(&images.Image{
						ID: imageID,
						Properties: map[string]any{
							imagePropertyForOS: "linux",
						},
					}, nil)
			},
			wantErr: "",
			verify: func(g Gomega, tpl *infrav1.OpenStackMachineTemplate) {
				// CPU = 4 cores
				expCPU := *resource.NewQuantity(4, resource.DecimalSI)
				g.Expect(tpl.Status.Capacity[corev1.ResourceCPU]).To(Equal(expCPU))

				// Memory = 8192 MiB → bytes
				ramBytes := int64(8192) * 1024 * 1024
				expMem := *resource.NewQuantity(ramBytes, resource.BinarySI)
				g.Expect(tpl.Status.Capacity[corev1.ResourceMemory]).To(Equal(expMem))

				// Ephemeral = 10 GiB → bytes
				ephBytes := int64(10) * 1024 * 1024 * 1024
				expEph := *resource.NewQuantity(ephBytes, resource.BinarySI)
				g.Expect(tpl.Status.Capacity[corev1.ResourceEphemeralStorage]).To(Equal(expEph))

				// Storage = Disk = 100 GiB → bytes (because RootVolume is set)
				storageBytes := int64(100) * 1024 * 1024 * 1024
				expStorage := *resource.NewQuantity(storageBytes, resource.BinarySI)
				g.Expect(tpl.Status.Capacity[corev1.ResourceStorage]).To(Equal(expStorage))

				// OS property
				g.Expect(tpl.Status.NodeInfo.OperatingSystem).To(Equal("linux"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			// fake k8s client (used by GetImageID)
			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				Build()

			// gomock controller
			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			// CAPO's MockScopeFactory (this is the key)
			mf := scope.NewMockScopeFactory(mockCtrl, "proj")

			// The scope this function expects:
			log := ctrl.Log.WithName("test")
			withLogger := scope.NewWithLogger(mf, log)

			// reconciler
			r := &OpenStackMachineTemplateReconciler{
				Client:       k8sClient,
				ScopeFactory: mf,
			}

			tpl := tt.tpl.DeepCopy()

			tt.expect(mf)

			err := r.reconcileNormal(ctx, withLogger, "test-cluster", tpl)

			if tt.wantErr == "" {
				g.Expect(err).ToNot(HaveOccurred())
				if tt.verify != nil {
					tt.verify(g, tpl)
				}
			} else {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(tt.wantErr))
			}
		})
	}
}

func newOSMT(name, clusterName string, paused bool, rootVolume bool, ownerRef bool) *infrav1.OpenStackMachineTemplate {
	osmt := &infrav1.OpenStackMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      name,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: clusterName,
			},
		},
		Spec: infrav1.OpenStackMachineTemplateSpec{
			Template: infrav1.OpenStackMachineTemplateResource{
				Spec: infrav1.OpenStackMachineSpec{
					FlavorID: ptr.To(flavorID),
					Image: infrav1.ImageParam{
						ID: &imageID,
					},
				},
			},
		},
	}

	if rootVolume {
		osmt.Spec.Template.Spec.RootVolume = &infrav1.RootVolume{
			SizeGiB: 100,
		}
	}

	if ownerRef {
		osmt.OwnerReferences = append(osmt.OwnerReferences, metav1.OwnerReference{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Cluster",
			Name:       clusterName,
			UID:        types.UID("291655c1-d923-4b50-8a3b-d552c03e33a7"),
		})
	}

	if paused {
		capiannotations.AddAnnotations(osmt, map[string]string{
			clusterv1.PausedAnnotation: "true",
		})
	}

	return osmt
}

func newCluster(name, infraName string, paused bool) *clusterv1.Cluster {
	c := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      name,
		},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrav1.GroupName,
				Kind:     "OpenStackCluster",
				Name:     infraName,
			},
		},
	}
	if paused {
		c.Spec.Paused = &paused
	}
	return c
}

func newOSCluster(name string) *infrav1.OpenStackCluster {
	return &infrav1.OpenStackCluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      name,
		},
	}
}

func TestOpenStackMachineTemplateReconciler_reconcileAllowedAddressPairs(t *testing.T) {
	const (
		ns          = "test-ns"
		clusterName = "test-cluster"
		osmtName    = "test-osmt"
		msName      = "test-ms"
		machineName = "test-machine"
		osmName     = "test-osm"
		portID      = "aaaa-bbbb-cccc-dddd"
	)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = infrav1.AddToScheme(scheme)

	// openStackMachine returns an OpenStackMachine with port portID in resources,
	// and currentPairs as the currently-resolved allowedAddressPairs.
	buildOSM := func(currentPairs []infrav1.AddressPair) *infrav1.OpenStackMachine {
		return &infrav1.OpenStackMachine{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: osmName},
			Status: infrav1.OpenStackMachineStatus{
				Resources: &infrav1.MachineResources{
					Ports: []infrav1.PortStatus{{ID: portID}},
				},
				Resolved: &infrav1.ResolvedMachineSpec{
					Ports: []infrav1.ResolvedPortSpec{
						{ResolvedPortSpecFields: infrav1.ResolvedPortSpecFields{
							AllowedAddressPairs: currentPairs,
						}},
					},
				},
			},
		}
	}

	// buildMachineSet returns a MachineSet that references osmtName as its infrastructure template.
	buildMachineSet := func() *clusterv1.MachineSet {
		return &clusterv1.MachineSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: msName,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
				UID:     types.UID("ms-uid"),
			},
			Spec: clusterv1.MachineSetSpec{
				ClusterName: clusterName,
				Selector:    metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
				Template: clusterv1.MachineTemplateSpec{
					Spec: clusterv1.MachineSpec{
						ClusterName: clusterName,
						InfrastructureRef: clusterv1.ContractVersionedObjectReference{
							Name: osmtName,
						},
					},
				},
			},
		}
	}

	// buildMachine returns a Machine owned by msName whose infra ref points to osmName.
	buildMachine := func() *clusterv1.Machine {
		return &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: machineName,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "MachineSet", Name: msName, UID: "ms-uid"},
				},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: clusterName,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					Name: osmName,
				},
			},
		}
	}

	// desiredPairs is what the template declares.
	desiredPairs := []infrav1.AddressPair{
		{IPAddress: "10.0.0.1"},
		{IPAddress: "10.0.0.2"},
	}

	// buildOSMT returns a template with one port carrying desiredPairs.
	buildOSMT := func() *infrav1.OpenStackMachineTemplate {
		return &infrav1.OpenStackMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: osmtName,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
			},
			Spec: infrav1.OpenStackMachineTemplateSpec{
				Template: infrav1.OpenStackMachineTemplateResource{
					Spec: infrav1.OpenStackMachineSpec{
						FlavorID: ptr.To(flavorID),
						Image:    infrav1.ImageParam{ID: &imageID},
						Ports: []infrav1.PortOpts{
							{ResolvedPortSpecFields: infrav1.ResolvedPortSpecFields{
								AllowedAddressPairs: desiredPairs,
							}},
						},
					},
				},
			},
		}
	}

	tests := []struct {
		name            string
		clusterNameArg  string
		osmt            *infrav1.OpenStackMachineTemplate
		extraObjects    []client.Object
		expectNetwork   func(m *scope.MockScopeFactory)
		wantErr         bool
		verify          func(g Gomega, cl client.Client)
	}{
		{
			name: "no ports in template - networking service never called",
			osmt: func() *infrav1.OpenStackMachineTemplate {
				t := buildOSMT()
				t.Spec.Template.Spec.Ports = nil
				return t
			}(),
			clusterNameArg: clusterName,
			extraObjects:   []client.Object{buildMachineSet(), buildMachine(), buildOSM(nil)},
			expectNetwork:  func(*scope.MockScopeFactory) {},
		},
		{
			name:           "empty clusterName - networking service never called",
			clusterNameArg: "",
			osmt:           buildOSMT(),
			extraObjects:   []client.Object{buildMachineSet(), buildMachine(), buildOSM(nil)},
			expectNetwork:  func(*scope.MockScopeFactory) {},
		},
		{
			name:           "no matching MachineSet - networking service never called",
			clusterNameArg: clusterName,
			osmt:           buildOSMT(),
			extraObjects:   []client.Object{buildMachine(), buildOSM(nil)},
			// MachineSet is absent → no machines found
			expectNetwork: func(*scope.MockScopeFactory) {},
		},
		{
			name:           "allowedAddressPairs already match - no UpdatePort call",
			clusterNameArg: clusterName,
			osmt:           buildOSMT(),
			extraObjects: []client.Object{
				buildMachineSet(),
				buildMachine(),
				buildOSM(desiredPairs), // already up to date
			},
			expectNetwork: func(*scope.MockScopeFactory) {},
		},
		{
			name:           "machine has no resolved status - no UpdatePort call",
			clusterNameArg: clusterName,
			osmt:           buildOSMT(),
			extraObjects: []client.Object{
				buildMachineSet(),
				buildMachine(),
				func() *infrav1.OpenStackMachine {
					osm := buildOSM(nil)
					osm.Status.Resolved = nil
					return osm
				}(),
			},
			expectNetwork: func(*scope.MockScopeFactory) {},
		},
		{
			name:           "allowedAddressPairs differ - UpdatePort called and machine status updated",
			clusterNameArg: clusterName,
			osmt:           buildOSMT(),
			extraObjects: []client.Object{
				buildMachineSet(),
				buildMachine(),
				buildOSM([]infrav1.AddressPair{{IPAddress: "1.2.3.4"}}), // stale
			},
			expectNetwork: func(mf *scope.MockScopeFactory) {
				mf.NetworkClient.EXPECT().
					UpdatePort(portID, gomock.Any()).
					Return(&ports.Port{ID: portID}, nil)
			},
			verify: func(g Gomega, cl client.Client) {
				// The machine's resolved status should now carry the desired pairs.
				osm := &infrav1.OpenStackMachine{}
				g.Expect(cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: osmName}, osm)).To(Succeed())
				g.Expect(osm.Status.Resolved.Ports[0].AllowedAddressPairs).To(ConsistOf(desiredPairs))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			allObjects := append([]client.Object{tt.osmt}, tt.extraObjects...)
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(allObjects...).
				WithStatusSubresource(&infrav1.OpenStackMachine{}).
				Build()

			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			mf := scope.NewMockScopeFactory(mockCtrl, "proj")
			tt.expectNetwork(mf)

			log := ctrl.Log.WithName("test")
			withLogger := scope.NewWithLogger(mf, log)

			r := &OpenStackMachineTemplateReconciler{
				Client:       cl,
				ScopeFactory: mf,
			}

			err := r.reconcileAllowedAddressPairs(context.Background(), withLogger, tt.clusterNameArg, tt.osmt)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}

			if tt.verify != nil {
				tt.verify(g, cl)
			}
		})
	}
}
