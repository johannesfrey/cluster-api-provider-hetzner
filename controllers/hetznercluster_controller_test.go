/*
Copyright 2022 The Kubernetes Authors.

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
	"time"

	"github.com/hetznercloud/hcloud-go/hcloud"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/syself/cluster-api-provider-hetzner/api/v1beta1"
	"github.com/syself/cluster-api-provider-hetzner/pkg/utils"
	"github.com/syself/cluster-api-provider-hetzner/test/helpers"
)

var _ = Describe("Hetzner ClusterReconciler", func() {
	It("should create a basic cluster", func() {
		// Create the secret
		hetznerSecret := getDefaultHetznerSecret("default")
		Expect(testEnv.Create(ctx, hetznerSecret)).To(Succeed())
		defer func() {
			Expect(testEnv.Cleanup(ctx, hetznerSecret)).To(Succeed())
		}()

		// Create the HetznerCluster object
		instance := &infrav1.HetznerCluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "hetzner-test1",
				Namespace:    "default",
			},
			Spec: getDefaultHetznerClusterSpec(),
		}
		Expect(testEnv.Create(ctx, instance)).To(Succeed())
		defer func() {
			Expect(testEnv.Delete(ctx, instance)).To(Succeed())
		}()

		key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

		// Create capi cluster
		capiCluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test1-",
				Namespace:    "default",
				Finalizers:   []string{clusterv1.ClusterFinalizer},
			},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{
					APIVersion: infrav1.GroupVersion.String(),
					Kind:       "HetznerCluster",
					Name:       instance.Name,
				},
			},
		}
		Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())
		defer func() {
			Expect(testEnv.Cleanup(ctx, capiCluster)).To(Succeed())
		}()

		// Make sure the HetznerCluster exists.
		Eventually(func() error {
			return testEnv.Get(ctx, key, instance)
		}, timeout).Should(BeNil())

		By("setting the OwnerRef on the HetznerCluster")
		// Set owner reference to Hetzner cluster
		Eventually(func() error {
			ph, err := patch.NewHelper(instance, testEnv)
			Expect(err).ShouldNot(HaveOccurred())
			instance.OwnerReferences = append(instance.OwnerReferences, metav1.OwnerReference{
				Kind:       "Cluster",
				APIVersion: clusterv1.GroupVersion.String(),
				Name:       capiCluster.Name,
				UID:        capiCluster.UID,
			})
			return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
		}, timeout).Should(BeNil())

		// Check whether finalizer has been set for HetznerCluster
		Eventually(func() bool {
			if err := testEnv.Get(ctx, key, instance); err != nil {
				return false
			}
			return len(instance.Finalizers) > 0
		}, timeout, time.Second).Should(BeTrue())
	})

	Context("load balancer", func() {
		var (
			err             error
			namespace       string
			testNs          *corev1.Namespace
			hetznerSecret   *corev1.Secret
			bootstrapSecret *corev1.Secret

			instance    *infrav1.HetznerCluster
			capiCluster *clusterv1.Cluster

			lbName string
		)
		BeforeEach(func() {
			testNs, err = testEnv.CreateNamespace(ctx, "lb-attachement")
			Expect(err).NotTo(HaveOccurred())
			namespace = testNs.Name

			lbName = utils.GenerateName(nil, "myloadbalancer")

			// Create the secret
			hetznerSecret = getDefaultHetznerSecret(namespace)
			Expect(testEnv.Create(ctx, hetznerSecret)).To(Succeed())

			hetznerClusterName := utils.GenerateName(nil, "hetzner-test1")
			// Create capi cluster
			capiCluster = &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test1-",
					Namespace:    namespace,
					Finalizers:   []string{clusterv1.ClusterFinalizer},
				},
				Spec: clusterv1.ClusterSpec{
					InfrastructureRef: &corev1.ObjectReference{
						APIVersion: infrav1.GroupVersion.String(),
						Kind:       "HetznerCluster",
						Name:       hetznerClusterName,
					},
				},
			}
			Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())

			// Create the HetznerCluster object
			instance = &infrav1.HetznerCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      hetznerClusterName,
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "cluster.x-k8s.io/v1beta1",
							Kind:       "Cluster",
							Name:       capiCluster.Name,
							UID:        capiCluster.UID,
						},
					},
				},
				Spec: getDefaultHetznerClusterSpec(),
			}
		})

		AfterEach(func() {
			Expect(testEnv.Cleanup(ctx, testNs, capiCluster, hetznerSecret, instance)).To(Succeed())
		})

		It("should create load balancer and update it accordingly", func() {
			Expect(testEnv.Create(ctx, instance)).To(Succeed())

			key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

			Eventually(func() bool {
				if err := testEnv.Get(ctx, key, instance); err != nil {
					return false
				}
				return isPresentAndTrue(key, instance, infrav1.LoadBalancerReadyCondition)
			}, timeout).Should(BeTrue())

			By("updating load balancer specs")
			newLBName := "new-lb-name"
			newLBType := "lb31"
			Eventually(func() error {
				ph, err := patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())
				instance.Spec.ControlPlaneLoadBalancer.Type = newLBType
				return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
			}, timeout).Should(BeNil())

			Eventually(func() error {
				ph, err := patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())

				instance.Spec.ControlPlaneLoadBalancer.Name = &newLBName
				return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
			}, timeout).Should(BeNil())

			// Check in hetzner API
			Eventually(func() error {
				loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{
					ListOpts: hcloud.ListOpts{
						LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
					},
				})
				if err != nil {
					return fmt.Errorf("failed to list load balancers: %w", err)
				}
				if len(loadBalancers) > 1 {
					return fmt.Errorf("there are multiple load balancers found: %v", loadBalancers)
				}
				if len(loadBalancers) == 0 {
					return fmt.Errorf("no load balancer found")
				}
				lb := loadBalancers[0]

				if lb.Name != newLBName {
					return fmt.Errorf("wrong name. Want %s, got %s", newLBName, lb.Name)
				}
				if lb.LoadBalancerType.Name != newLBType {
					return fmt.Errorf("wrong name. Want %s, got %s", newLBType, lb.LoadBalancerType.Name)
				}
				return nil
			}, timeout).Should(BeNil())

			By("Getting additional extra services")
			Eventually(func() error {
				ph, err := patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())
				instance.Spec.ControlPlaneLoadBalancer.ExtraServices = append(instance.Spec.ControlPlaneLoadBalancer.ExtraServices,
					infrav1.LoadBalancerServiceSpec{
						DestinationPort: 8134,
						ListenPort:      8134,
						Protocol:        "tcp",
					})
				return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
			}, timeout).Should(BeNil())

			Eventually(func() int {
				loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{
					ListOpts: hcloud.ListOpts{
						LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
					},
				})
				if err != nil {
					return -1
				}
				if len(loadBalancers) > 1 {
					return -2
				}
				if len(loadBalancers) == 0 {
					return -3
				}
				lb := loadBalancers[0]

				return len(lb.Services)
			}, timeout).Should(Equal(len(instance.Spec.ControlPlaneLoadBalancer.ExtraServices)))

			By("Getting reducing extra targets")
			Eventually(func() error {
				ph, err := patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())
				instance.Spec.ControlPlaneLoadBalancer.ExtraServices = []infrav1.LoadBalancerServiceSpec{
					{
						DestinationPort: 8134,
						ListenPort:      8134,
						Protocol:        "tcp",
					},
				}
				return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
			}, timeout).Should(BeNil())

			Eventually(func() int {
				loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{
					ListOpts: hcloud.ListOpts{
						LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
					},
				})
				if err != nil {
					return -1
				}
				if len(loadBalancers) > 1 {
					return -2
				}
				if len(loadBalancers) == 0 {
					return -3
				}
				lb := loadBalancers[0]

				return len(lb.Services)
			}, timeout).Should(Equal(len(instance.Spec.ControlPlaneLoadBalancer.ExtraServices)))

			By("Getting removing extra targets")
			Eventually(func() error {
				ph, err := patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())
				instance.Spec.ControlPlaneLoadBalancer.ExtraServices = nil
				return ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})
			}, timeout).Should(BeNil())

			Eventually(func() int {
				loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{
					ListOpts: hcloud.ListOpts{
						LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
					},
				})
				if err != nil {
					return -1
				}
				if len(loadBalancers) > 1 {
					return -2
				}
				if len(loadBalancers) == 0 {
					return -3
				}
				lb := loadBalancers[0]

				return len(lb.Services)
			}, timeout).Should(Equal(len(instance.Spec.ControlPlaneLoadBalancer.ExtraServices)))
		})

		It("should not create load balancer if disabled and the cluster should get ready", func() {
			// Create the bootstrap secret
			bootstrapSecret = getDefaultBootstrapSecret(namespace)
			Expect(testEnv.Create(ctx, bootstrapSecret)).To(Succeed())

			defer func() {
				Expect(testEnv.Delete(ctx, bootstrapSecret)).To(Succeed())
			}()

			instance.Spec.ControlPlaneLoadBalancer.Enabled = false
			instance.Spec.ControlPlaneEndpoint = &clusterv1.APIEndpoint{
				Host: "my.test.host",
				Port: 6443,
			}
			Expect(testEnv.Create(ctx, instance)).To(Succeed())

			key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}
			Eventually(func() bool {
				if err := testEnv.Get(ctx, key, instance); err != nil {
					return false
				}

				return instance.Status.ControlPlaneLoadBalancer == nil && instance.Status.Ready
			}, timeout, time.Second).Should(BeTrue())
		})

		It("should take over an existing load balancer with correct name", func() {
			// Create the bootstrap secret
			bootstrapSecret = getDefaultBootstrapSecret(namespace)
			Expect(testEnv.Create(ctx, bootstrapSecret)).To(Succeed())

			defer func() {
				Expect(testEnv.Delete(ctx, bootstrapSecret)).To(Succeed())
			}()

			hcloudClient := testEnv.HCloudClientFactory.NewClient("")

			By("creating load balancer manually")

			opts := hcloud.LoadBalancerCreateOpts{
				Name:             lbName,
				Algorithm:        &hcloud.LoadBalancerAlgorithm{Type: hcloud.LoadBalancerAlgorithmTypeLeastConnections},
				LoadBalancerType: &hcloud.LoadBalancerType{Name: "mytype"},
			}

			_, err := hcloudClient.CreateLoadBalancer(ctx, opts)
			Expect(err).To(BeNil())

			By("creating cluster object")
			instance.Spec.ControlPlaneLoadBalancer.Name = &lbName
			Expect(testEnv.Create(ctx, instance)).To(Succeed())

			By("checking that cluster is ready")
			key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

			Eventually(func() bool {
				var foundInstance infrav1.HetznerCluster
				if err := testEnv.Get(ctx, key, &foundInstance); err != nil {
					return false
				}

				return isPresentAndTrue(key, instance, infrav1.LoadBalancerReadyCondition)
			}, timeout, time.Second).Should(BeTrue())

			By("checking that load balancer has label set")
			loadBalancers, err := hcloudClient.ListLoadBalancers(ctx, hcloud.LoadBalancerListOpts{Name: lbName})
			Expect(err).To(BeNil())
			Expect(loadBalancers).To(HaveLen(1))

			value, found := loadBalancers[0].Labels[instance.ClusterTagKey()]
			Expect(found).To(BeTrue())
			Expect(value).To(Equal(string(infrav1.ResourceLifecycleOwned)))
		})

		It("should set the appropriate condition if a named load balancer is taken by another cluster", func() {
			// Create the bootstrap secret
			bootstrapSecret = getDefaultBootstrapSecret(namespace)
			Expect(testEnv.Create(ctx, bootstrapSecret)).To(Succeed())

			defer func() {
				Expect(testEnv.Delete(ctx, bootstrapSecret)).To(Succeed())
			}()

			hcloudClient := testEnv.HCloudClientFactory.NewClient("")

			By("creating load balancer manually")
			labelsOwnedByOtherCluster := map[string]string{instance.ClusterTagKey() + "s": string(infrav1.ResourceLifecycleOwned)}
			opts := hcloud.LoadBalancerCreateOpts{
				Name:             lbName,
				Algorithm:        &hcloud.LoadBalancerAlgorithm{Type: hcloud.LoadBalancerAlgorithmTypeLeastConnections},
				LoadBalancerType: &hcloud.LoadBalancerType{Name: "mytype"},
				Labels:           labelsOwnedByOtherCluster,
			}

			_, err := hcloudClient.CreateLoadBalancer(ctx, opts)
			Expect(err).To(BeNil())

			By("creating cluster object")
			instance.Spec.ControlPlaneLoadBalancer.Name = &lbName
			Expect(testEnv.Create(ctx, instance)).To(Succeed())

			By("checking that cluster is ready")
			key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

			Eventually(func() bool {
				var foundInstance infrav1.HetznerCluster
				if err := testEnv.Get(ctx, key, &foundInstance); err != nil {
					return false
				}

				return isPresentAndFalseWithReason(key, instance, infrav1.LoadBalancerReadyCondition, infrav1.LoadBalancerFailedToOwnReason)
			}, timeout, time.Second).Should(BeTrue())
		})

		It("should set the appropriate condition if a named load balancer is not found", func() {
			// Create the bootstrap secret
			bootstrapSecret = getDefaultBootstrapSecret(namespace)
			Expect(testEnv.Create(ctx, bootstrapSecret)).To(Succeed())

			defer func() {
				Expect(testEnv.Delete(ctx, bootstrapSecret)).To(Succeed())
			}()

			By("creating cluster object")
			instance.Spec.ControlPlaneLoadBalancer.Name = &lbName
			Expect(testEnv.Create(ctx, instance)).To(Succeed())

			By("checking that cluster has condition set")
			key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

			Eventually(func() bool {
				var foundInstance infrav1.HetznerCluster
				if err := testEnv.Get(ctx, key, &foundInstance); err != nil {
					return false
				}

				return isPresentAndFalseWithReason(key, instance, infrav1.LoadBalancerReadyCondition, infrav1.LoadBalancerNotFoundReason)
			}, timeout, time.Second).Should(BeTrue())
		})
	})

	Context("For HetznerMachines belonging to the cluster", func() {
		var (
			namespace       string
			testNs          *corev1.Namespace
			hetznerSecret   *corev1.Secret
			bootstrapSecret *corev1.Secret
		)

		BeforeEach(func() {
			var err error
			testNs, err = testEnv.CreateNamespace(ctx, "hetzner-owner-ref")
			Expect(err).NotTo(HaveOccurred())
			namespace = testNs.Name

			// Create the hetzner secret
			hetznerSecret = getDefaultHetznerSecret(namespace)
			Expect(testEnv.Create(ctx, hetznerSecret)).To(Succeed())
			// Create the bootstrap secret
			bootstrapSecret = getDefaultBootstrapSecret(namespace)
			Expect(testEnv.Create(ctx, bootstrapSecret)).To(Succeed())
		})

		AfterEach(func() {
			Expect(testEnv.Delete(ctx, bootstrapSecret)).To(Succeed())
			Expect(testEnv.Delete(ctx, hetznerSecret)).To(Succeed())
			Expect(testEnv.Delete(ctx, testNs)).To(Succeed())
		})

		It("sets owner references to those machines", func() {
			// Create the HetznerCluster object
			instance := &infrav1.HetznerCluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test1-",
					Namespace:    namespace,
				},
				Spec: getDefaultHetznerClusterSpec(),
			}
			Expect(testEnv.Create(ctx, instance)).To(Succeed())
			defer func() {
				Expect(testEnv.Cleanup(ctx, instance)).To(Succeed())
			}()
			capiCluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "capi-test1-",
					Namespace:    namespace,
					Finalizers:   []string{clusterv1.ClusterFinalizer},
				},
				Spec: clusterv1.ClusterSpec{
					InfrastructureRef: &corev1.ObjectReference{
						APIVersion: infrav1.GroupVersion.String(),
						Kind:       "HetznerCluster",
						Name:       instance.Name,
						Namespace:  namespace,
					},
				},
			}
			Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())
			defer func() {
				Expect(testEnv.Cleanup(ctx, capiCluster)).To(Succeed())
			}()

			// Make sure the HCloudCluster exists.
			Eventually(func() error {
				return testEnv.Get(ctx, client.ObjectKey{Namespace: namespace, Name: instance.Name}, instance)
			}, timeout).Should(BeNil())

			// Create machines
			machineCount := 3
			for i := 0; i < machineCount; i++ {
				Expect(createHCloudMachine(ctx, testEnv, namespace, capiCluster.Name)).To(Succeed())
			}

			// Set owner reference to HetznerCluster
			Eventually(func() bool {
				ph, err := patch.NewHelper(instance, testEnv)
				Expect(err).ShouldNot(HaveOccurred())
				instance.OwnerReferences = append(instance.OwnerReferences, metav1.OwnerReference{
					Kind:       "Cluster",
					APIVersion: clusterv1.GroupVersion.String(),
					Name:       capiCluster.Name,
					UID:        capiCluster.UID,
				})
				Expect(ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})).ShouldNot(HaveOccurred())
				return true
			}, timeout).Should(BeTrue())

			By("checking for presence of HCloudMachine objects")
			// Check if machines have been created
			Eventually(func() int {
				servers, err := hcloudClient.ListServers(ctx, hcloud.ServerListOpts{
					ListOpts: hcloud.ListOpts{
						LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
					},
				})
				if err != nil {
					return -1
				}
				return len(servers)
			}, timeout).Should(Equal(machineCount))
		})
	})

	Context("Placement groups", func() {
		var (
			namespace       string
			testNs          *corev1.Namespace
			hetznerSecret   *corev1.Secret
			bootstrapSecret *corev1.Secret

			instance    *infrav1.HetznerCluster
			capiCluster *clusterv1.Cluster
		)

		BeforeEach(func() {
			var err error
			testNs, err = testEnv.CreateNamespace(ctx, "ns-placement-groups")
			Expect(err).NotTo(HaveOccurred())
			namespace = testNs.Name

			// Create the hetzner secret
			hetznerSecret = getDefaultHetznerSecret(namespace)
			Expect(testEnv.Create(ctx, hetznerSecret)).To(Succeed())
			// Create the bootstrap secret
			bootstrapSecret = getDefaultBootstrapSecret(namespace)
			Expect(testEnv.Create(ctx, bootstrapSecret)).To(Succeed())

			hetznerClusterName := utils.GenerateName(nil, "test1-")

			capiCluster = &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "capi-test1-",
					Namespace:    namespace,
					Finalizers:   []string{clusterv1.ClusterFinalizer},
				},
				Spec: clusterv1.ClusterSpec{
					InfrastructureRef: &corev1.ObjectReference{
						APIVersion: infrav1.GroupVersion.String(),
						Kind:       "HetznerCluster",
						Name:       hetznerClusterName,
						Namespace:  namespace,
					},
				},
			}
			Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())

			instance = &infrav1.HetznerCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      hetznerClusterName,
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "cluster.x-k8s.io/v1beta1",
							Kind:       "Cluster",
							Name:       capiCluster.Name,
							UID:        capiCluster.UID,
						},
					},
				},
				Spec: getDefaultHetznerClusterSpec(),
			}
		})

		AfterEach(func() {
			Expect(testEnv.Delete(ctx, bootstrapSecret)).To(Succeed())
			Expect(testEnv.Delete(ctx, hetznerSecret)).To(Succeed())
			Expect(testEnv.Delete(ctx, capiCluster)).To(Succeed())
			Expect(testEnv.Delete(ctx, testNs)).To(Succeed())
		})

		DescribeTable("create and delete placement groups without error",
			func(placementGroups []infrav1.HCloudPlacementGroupSpec) {
				// Create the HetznerCluster object
				instance.Spec.HCloudPlacementGroups = placementGroups
				Expect(testEnv.Create(ctx, instance)).To(Succeed())
				defer func() {
					Expect(testEnv.Cleanup(ctx, instance)).To(Succeed())
				}()

				key := client.ObjectKey{Namespace: namespace, Name: instance.Name}

				Eventually(func() bool {
					if err := testEnv.Get(ctx, key, instance); err != nil {
						return false
					}
					return isPresentAndTrue(key, instance, infrav1.PlacementGroupsSyncedCondition)
				}, timeout).Should(BeTrue())

				By("checking for presence of HCloudPlacementGroup objects")
				// Check if placement groups have been created
				Eventually(func() int {
					pgs, err := hcloudClient.ListPlacementGroups(ctx, hcloud.PlacementGroupListOpts{
						ListOpts: hcloud.ListOpts{
							LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
						},
					})
					if err != nil {
						return -1
					}
					return len(pgs)
				}, timeout).Should(Equal(len(placementGroups)))
			},
			Entry("placement groups", []infrav1.HCloudPlacementGroupSpec{
				{
					Name: defaultPlacementGroupName,
					Type: "spread",
				},
				{
					Name: "md-0",
					Type: "spread",
				},
			}),
			Entry("no placement groups", []infrav1.HCloudPlacementGroupSpec{}),
		)

		Describe("update placement groups", func() {
			BeforeEach(func() {
				Expect(testEnv.Create(ctx, instance)).To(Succeed())
			})
			AfterEach(func() {
				Expect(testEnv.Cleanup(ctx, instance)).To(Succeed())
			})

			DescribeTable("update placement groups",
				func(newPlacementGroupSpec []infrav1.HCloudPlacementGroupSpec) {
					ph, err := patch.NewHelper(instance, testEnv)
					Expect(err).ShouldNot(HaveOccurred())
					instance.Spec.HCloudPlacementGroups = newPlacementGroupSpec
					Expect(ph.Patch(ctx, instance, patch.WithStatusObservedGeneration{})).To(Succeed())

					Eventually(func() int {
						pgs, err := hcloudClient.ListPlacementGroups(ctx, hcloud.PlacementGroupListOpts{
							ListOpts: hcloud.ListOpts{
								LabelSelector: utils.LabelsToLabelSelector(map[string]string{instance.ClusterTagKey(): "owned"}),
							},
						})
						if err != nil {
							return -1
						}
						return len(pgs)
					}, timeout).Should(Equal(len(newPlacementGroupSpec)))
				},
				Entry("one pg", []infrav1.HCloudPlacementGroupSpec{{Name: "md-0", Type: "spread"}}),
				Entry("no pgs", []infrav1.HCloudPlacementGroupSpec{}),
				Entry("three pgs", []infrav1.HCloudPlacementGroupSpec{
					{Name: "md-0", Type: "spread"},
					{Name: "md-1", Type: "spread"},
					{Name: "md-2", Type: "spread"},
				}),
			)
		})
	})
	Context("network", func() {
		var (
			namespace       string
			testNs          *corev1.Namespace
			bootstrapSecret *corev1.Secret
			hetznerSecret   *corev1.Secret
		)

		hetznerClusterSpecWithDisabledNetwork := getDefaultHetznerClusterSpec()
		hetznerClusterSpecWithDisabledNetwork.HCloudNetwork.Enabled = false
		hetznerClusterSpecWithoutNetwork := getDefaultHetznerClusterSpec()
		hetznerClusterSpecWithoutNetwork.HCloudNetwork = infrav1.HCloudNetworkSpec{}
		hetznerClusterSpecWithNetworkID := getDefaultHetznerClusterSpec()
		hetznerClusterSpecWithNetworkID.HCloudNetwork.ID = pointer.Int(1)

		BeforeEach(func() {
			var err error
			testNs, err = testEnv.CreateNamespace(ctx, "ns-network")
			Expect(err).NotTo(HaveOccurred())
			namespace = testNs.Name

			// Create the bootstrap secret
			bootstrapSecret = getDefaultBootstrapSecret(namespace)
			Expect(testEnv.Create(ctx, bootstrapSecret)).To(Succeed())
			// Create the hetzner secret
			hetznerSecret = getDefaultHetznerSecret(namespace)
			Expect(testEnv.Create(ctx, hetznerSecret)).To(Succeed())
		})

		AfterEach(func() {
			Expect(testEnv.Delete(ctx, bootstrapSecret)).To(Succeed())
			Expect(testEnv.Delete(ctx, hetznerSecret)).To(Succeed())
			Expect(testEnv.Delete(ctx, testNs)).To(Succeed())
		})

		DescribeTable("toggle network",
			func(hetznerClusterSpec infrav1.HetznerClusterSpec, expectedConditionState bool, expectedReason string) {
				hetznerClusterName := utils.GenerateName(nil, "test1-")
				capiCluster := &clusterv1.Cluster{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "capi-test1-",
						Namespace:    namespace,
						Finalizers:   []string{clusterv1.ClusterFinalizer},
					},
					Spec: clusterv1.ClusterSpec{
						InfrastructureRef: &corev1.ObjectReference{
							APIVersion: infrav1.GroupVersion.String(),
							Kind:       "HetznerCluster",
							Name:       hetznerClusterName,
							Namespace:  namespace,
						},
					},
				}
				Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())
				defer func() {
					Expect(testEnv.Cleanup(ctx, capiCluster)).To(Succeed())
				}()

				// Create the HetznerCluster object
				instance := &infrav1.HetznerCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      hetznerClusterName,
						Namespace: namespace,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "cluster.x-k8s.io/v1beta1",
								Kind:       "Cluster",
								Name:       capiCluster.Name,
								UID:        capiCluster.UID,
							},
						},
					},
					Spec: hetznerClusterSpec,
				}
				Expect(testEnv.Create(ctx, instance)).To(Succeed())
				defer func() {
					Expect(testEnv.Cleanup(ctx, instance)).To(Succeed())
				}()

				key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

				Eventually(func() bool {
					if err := testEnv.Get(ctx, key, instance); err != nil {
						return false
					}
					if expectedConditionState {
						return isPresentAndTrue(key, instance, infrav1.NetworkAttachedCondition)
					}
					return isPresentAndFalseWithReason(key, instance, infrav1.NetworkAttachedCondition, expectedReason)
				}, timeout).Should(BeTrue())
			},
			Entry("with network", getDefaultHetznerClusterSpec(), true, ""),
			Entry("with network ID", hetznerClusterSpecWithNetworkID, true, ""),
		)

		Describe("For an existing Network", func() {
			It("should attach the existing unlabeled Network by ID and not create a new one", func() {
				networkName := utils.GenerateName(nil, "network1-")
				network, err := hcloudClient.CreateNetwork(context.Background(), hcloud.NetworkCreateOpts{Name: networkName})
				Expect(err).To(Succeed())
				defer func() {
					err := hcloudClient.DeleteNetwork(context.Background(), network)
					Expect(err).To(Succeed())
				}()
				networksBeforeClusterCreate, err := hcloudClient.ListNetworks(context.Background(), hcloud.NetworkListOpts{})
				Expect(err).To(Succeed())

				hetznerClusterName := utils.GenerateName(nil, "test1-")

				capiCluster := &clusterv1.Cluster{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "capi-test1-",
						Namespace:    namespace,
						Finalizers:   []string{clusterv1.ClusterFinalizer},
					},
					Spec: clusterv1.ClusterSpec{
						InfrastructureRef: &corev1.ObjectReference{
							APIVersion: infrav1.GroupVersion.String(),
							Kind:       "HetznerCluster",
							Name:       hetznerClusterName,
							Namespace:  namespace,
						},
					},
				}
				Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())
				defer func() {
					Expect(testEnv.Cleanup(ctx, capiCluster)).To(Succeed())
				}()

				instance := &infrav1.HetznerCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      hetznerClusterName,
						Namespace: namespace,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "cluster.x-k8s.io/v1beta1",
								Kind:       "Cluster",
								Name:       capiCluster.Name,
								UID:        capiCluster.UID,
							},
						},
					},
					Spec: getDefaultHetznerClusterSpec(),
				}
				// the creation of a HetznerCluster should not lead to the creation of a network when the ID of an
				// existing network was given.
				instance.Spec.HCloudNetwork.ID = pointer.Int(network.ID)
				Expect(testEnv.Create(ctx, instance)).To(Succeed())
				defer func() {
					Expect(testEnv.Cleanup(ctx, instance)).To(Succeed())
				}()

				key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

				Eventually(func() bool {
					if err := testEnv.Get(ctx, key, instance); err != nil {
						return false
					}
					if isPresentAndTrue(key, instance, infrav1.NetworkAttachedCondition) && instance.Status.Network != nil {
						return true
					}
					return false
				}, timeout).Should(BeTrue())

				networks, err := hcloudClient.ListNetworks(ctx, hcloud.NetworkListOpts{})
				Expect(err).To(Succeed())

				Expect(len(networks)).To(Equal(len(networksBeforeClusterCreate)))
			})
			It("should not delete the existing unlabeled Network when deleting the Cluster", func() {
				networkName := utils.GenerateName(nil, "network2-")
				network, err := hcloudClient.CreateNetwork(context.Background(), hcloud.NetworkCreateOpts{Name: networkName})
				Expect(err).To(Succeed())
				defer func() {
					err := hcloudClient.DeleteNetwork(context.Background(), network)
					Expect(err).To(Succeed())
				}()
				networksBeforeClusterDelete, err := hcloudClient.ListNetworks(context.Background(), hcloud.NetworkListOpts{})
				Expect(err).To(Succeed())

				hetznerClusterName := utils.GenerateName(nil, "test1-")

				capiCluster := &clusterv1.Cluster{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "capi-test1-",
						Namespace:    namespace,
						Finalizers:   []string{clusterv1.ClusterFinalizer},
					},
					Spec: clusterv1.ClusterSpec{
						InfrastructureRef: &corev1.ObjectReference{
							APIVersion: infrav1.GroupVersion.String(),
							Kind:       "HetznerCluster",
							Name:       hetznerClusterName,
							Namespace:  namespace,
						},
					},
				}
				Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())

				instance := &infrav1.HetznerCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      hetznerClusterName,
						Namespace: namespace,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "cluster.x-k8s.io/v1beta1",
								Kind:       "Cluster",
								Name:       capiCluster.Name,
								UID:        capiCluster.UID,
							},
						},
					},
					Spec: getDefaultHetznerClusterSpec(),
				}
				instance.Spec.HCloudNetwork.ID = pointer.Int(network.ID)
				Expect(testEnv.Create(ctx, instance)).To(Succeed())

				key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

				Eventually(func() bool {
					if err := testEnv.Get(ctx, key, instance); err != nil {
						return false
					}
					if isPresentAndTrue(key, instance, infrav1.NetworkAttachedCondition) && instance.Status.Network != nil {
						return true
					}
					return false
				}, timeout).Should(BeTrue())

				// the deletion of a HetznerCluster should not lead to the deletion of an existing network
				// when the network misses the `owned` label.
				Expect(testEnv.Cleanup(ctx, instance, capiCluster)).To(Succeed())

				Eventually(func() bool {
					if err := testEnv.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}, instance); err != nil && apierrors.IsNotFound(err) {
						return true
					} else if err != nil {
						return false
					}
					return false
				}, timeout).Should(BeTrue())

				networks, err := hcloudClient.ListNetworks(ctx, hcloud.NetworkListOpts{})
				Expect(err).To(Succeed())

				Expect(len(networks)).To(Equal(len(networksBeforeClusterDelete)))
			})
			It(`should delete the existing "owned" labeled Network when deleting the Cluster`, func() {
				hetznerClusterName := utils.GenerateName(nil, "test1-")

				capiCluster := &clusterv1.Cluster{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "capi-test1-",
						Namespace:    namespace,
						Finalizers:   []string{clusterv1.ClusterFinalizer},
					},
					Spec: clusterv1.ClusterSpec{
						InfrastructureRef: &corev1.ObjectReference{
							APIVersion: infrav1.GroupVersion.String(),
							Kind:       "HetznerCluster",
							Name:       hetznerClusterName,
							Namespace:  namespace,
						},
					},
				}
				Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())

				instance := &infrav1.HetznerCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      hetznerClusterName,
						Namespace: namespace,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "cluster.x-k8s.io/v1beta1",
								Kind:       "Cluster",
								Name:       capiCluster.Name,
								UID:        capiCluster.UID,
							},
						},
					},
					Spec: getDefaultHetznerClusterSpec(),
				}

				networkName := utils.GenerateName(nil, "network3-")
				network, err := hcloudClient.CreateNetwork(context.Background(), hcloud.NetworkCreateOpts{
					Name:   networkName,
					Labels: map[string]string{instance.ClusterTagKey(): "owned"},
				})
				Expect(err).To(Succeed())

				networksBeforeClusterDelete, err := hcloudClient.ListNetworks(context.Background(), hcloud.NetworkListOpts{})
				Expect(err).To(Succeed())

				instance.Spec.HCloudNetwork.ID = pointer.Int(network.ID)
				Expect(testEnv.Create(ctx, instance)).To(Succeed())

				key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}

				Eventually(func() bool {
					if err := testEnv.Get(ctx, key, instance); err != nil {
						return false
					}
					if isPresentAndTrue(key, instance, infrav1.NetworkAttachedCondition) && instance.Status.Network != nil {
						return true
					}
					return false
				}, timeout).Should(BeTrue())

				// As the network has the `owned` label, the deletion of a HetznerCluster will also lead to the
				// deletion of the network.
				Expect(testEnv.Cleanup(ctx, instance, capiCluster)).To(Succeed())

				Eventually(func() bool {
					if err := testEnv.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: instance.Name}, instance); err != nil && apierrors.IsNotFound(err) {
						return true
					} else if err != nil {
						return false
					}
					return false
				}, timeout).Should(BeTrue())

				networks, err := hcloudClient.ListNetworks(ctx, hcloud.NetworkListOpts{})
				Expect(err).To(Succeed())

				Expect(len(networks)).To(Equal(len(networksBeforeClusterDelete) - 1))
			})
		})
	})
})

func createHCloudMachine(ctx context.Context, env *helpers.TestEnvironment, namespace, clusterName string) error {
	hcloudMachineName := utils.GenerateName(nil, "hcloud-machine")
	capiMachine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "capi-machine-",
			Namespace:    namespace,
			Finalizers:   []string{clusterv1.MachineFinalizer},
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: clusterName,
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			InfrastructureRef: corev1.ObjectReference{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Kind:       "HCloudMachine",
				Name:       hcloudMachineName,
			},
			FailureDomain: &defaultFailureDomain,
			Bootstrap: clusterv1.Bootstrap{
				DataSecretName: pointer.String("bootstrap-secret"),
			},
		},
	}
	if err := env.Create(ctx, capiMachine); err != nil {
		return err
	}

	hcloudMachine := &infrav1.HCloudMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hcloudMachineName,
			Namespace: namespace,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: clusterName},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Machine",
					Name:       capiMachine.Name,
					UID:        capiMachine.UID,
				},
			},
		},
		Spec: infrav1.HCloudMachineSpec{
			ImageName: "fedora-control-plane",
			Type:      "cpx31",
		},
	}
	return env.Create(ctx, hcloudMachine)
}

var _ = Describe("Hetzner secret", func() {
	var (
		hetznerCluster     *infrav1.HetznerCluster
		capiCluster        *clusterv1.Cluster
		key                client.ObjectKey
		hetznerSecret      *corev1.Secret
		hetznerClusterName string
	)

	BeforeEach(func() {
		var err error
		Expect(err).NotTo(HaveOccurred())

		hetznerClusterName = utils.GenerateName(nil, "hetzner-cluster-test")
		capiCluster = &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test1-",
				Namespace:    "default",
				Finalizers:   []string{clusterv1.ClusterFinalizer},
			},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{
					APIVersion: infrav1.GroupVersion.String(),
					Kind:       "HetznerCluster",
					Name:       hetznerClusterName,
					Namespace:  "default",
				},
			},
		}
		Expect(testEnv.Create(ctx, capiCluster)).To(Succeed())

		hetznerCluster = &infrav1.HetznerCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      hetznerClusterName,
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "cluster.x-k8s.io/v1beta1",
						Kind:       "Cluster",
						Name:       capiCluster.Name,
						UID:        capiCluster.UID,
					},
				},
			},
			Spec: getDefaultHetznerClusterSpec(),
		}
		Expect(testEnv.Create(ctx, hetznerCluster)).To(Succeed())

		key = client.ObjectKey{Namespace: hetznerCluster.Namespace, Name: hetznerCluster.Name}
	})

	AfterEach(func() {
		Expect(testEnv.Cleanup(ctx, hetznerCluster, capiCluster, hetznerSecret)).To(Succeed())

		Eventually(func() bool {
			if err := testEnv.Get(ctx, client.ObjectKey{Namespace: hetznerSecret.Namespace, Name: hetznerSecret.Name}, hetznerSecret); err != nil && apierrors.IsNotFound(err) {
				return true
			} else if err != nil {
				return false
			}
			// Secret still there, so the finalizers have not been removed. Patch to remove them.
			ph, err := patch.NewHelper(hetznerSecret, testEnv)
			Expect(err).ShouldNot(HaveOccurred())
			hetznerSecret.Finalizers = nil
			Expect(ph.Patch(ctx, hetznerSecret, patch.WithStatusObservedGeneration{})).To(Succeed())
			// Should delete secret
			if err := testEnv.Delete(ctx, hetznerSecret); err != nil && apierrors.IsNotFound(err) {
				// Has been deleted already
				return true
			}
			return false
		}, time.Second, time.Second).Should(BeTrue())
	})

	DescribeTable("test different hetzner secret",
		func(secret corev1.Secret, expectedReason string) {
			hetznerSecret = &secret
			Expect(testEnv.Create(ctx, hetznerSecret)).To(Succeed())

			Eventually(func() bool {
				if err := testEnv.Get(ctx, key, hetznerCluster); err != nil {
					return false
				}
				return isPresentAndFalseWithReason(key, hetznerCluster, infrav1.HCloudTokenAvailableCondition, expectedReason)
			}, timeout, time.Second).Should(BeTrue())
		},
		Entry("no Hetzner secret/wrong reference", corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "wrong-name",
				Namespace: "default",
			},
			Data: map[string][]byte{
				"hcloud": []byte("my-token"),
			},
		}, infrav1.HetznerSecretUnreachableReason),
		Entry("empty hcloud token", corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "hetzner-secret",
				Namespace: "default",
			},
			Data: map[string][]byte{
				"hcloud": []byte(""),
			},
		}, infrav1.HCloudCredentialsInvalidReason),
		Entry("wrong key in secret", corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "hetzner-secret",
				Namespace: "default",
			},
			Data: map[string][]byte{
				"wrongkey": []byte("my-token"),
			},
		}, infrav1.HCloudCredentialsInvalidReason),
	)
})

var _ = Describe("HetznerCluster validation", func() {
	var (
		hetznerCluster *infrav1.HetznerCluster
		testNs         *corev1.Namespace
	)
	BeforeEach(func() {
		var err error
		testNs, err = testEnv.CreateNamespace(ctx, "hcloudmachine-validation")
		Expect(err).NotTo(HaveOccurred())
	})
	AfterEach(func() {
		Expect(testEnv.Cleanup(ctx, testNs, hetznerCluster)).To(Succeed())
	})

	Context("validate create", func() {
		BeforeEach(func() {
			hetznerCluster = &infrav1.HetznerCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hcloud-validation-machine",
					Namespace: testNs.Name,
				},
				Spec: getDefaultHetznerClusterSpec(),
			}
		})

		It("should fail without a wrong controlPlaneRegion name", func() {
			hetznerCluster.Spec.ControlPlaneRegions = append(hetznerCluster.Spec.ControlPlaneRegions, infrav1.Region("wrong-region"))
			Expect(testEnv.Create(ctx, hetznerCluster)).ToNot(Succeed())
		})

		It("should fail with an SSHKey without name", func() {
			hetznerCluster.Spec.SSHKeys.HCloud = append(hetznerCluster.Spec.SSHKeys.HCloud, infrav1.SSHKey{})
			Expect(testEnv.Create(ctx, hetznerCluster)).ToNot(Succeed())
		})

		It("should fail with an empty controlPlaneLoadBalancer region", func() {
			hetznerCluster.Spec.ControlPlaneLoadBalancer.Region = ""
			Expect(testEnv.Create(ctx, hetznerCluster)).ToNot(Succeed())
		})

		It("should fail with an empty placementGroup name", func() {
			hetznerCluster.Spec.HCloudPlacementGroups = append(hetznerCluster.Spec.HCloudPlacementGroups, infrav1.HCloudPlacementGroupSpec{})
			Expect(testEnv.Create(ctx, hetznerCluster)).ToNot(Succeed())
		})

		It("should fail with a wrong placementGroup type", func() {
			hetznerCluster.Spec.HCloudPlacementGroups = append(hetznerCluster.Spec.HCloudPlacementGroups, infrav1.HCloudPlacementGroupSpec{
				Name: "newName",
				Type: "wrong-type",
			})
			Expect(testEnv.Create(ctx, hetznerCluster)).ToNot(Succeed())
		})
	})
})

var _ = Describe("reconcileRateLimit", func() {
	var hetznerCluster *infrav1.HetznerCluster
	BeforeEach(func() {
		hetznerCluster = &infrav1.HetznerCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rate-limit-cluster",
				Namespace: "default",
			},
			Spec: getDefaultHetznerClusterSpec(),
		}
	})

	It("returns wait== true if rate limit exceeded is set and time is not over", func() {
		conditions.MarkFalse(hetznerCluster, infrav1.HetznerAPIReachableCondition, infrav1.RateLimitExceededReason, clusterv1.ConditionSeverityWarning, "")
		Expect(reconcileRateLimit(hetznerCluster)).To(BeTrue())
	})

	It("returns wait== false if rate limit exceeded is set and time is over", func() {
		conditions.MarkFalse(hetznerCluster, infrav1.HetznerAPIReachableCondition, infrav1.RateLimitExceededReason, clusterv1.ConditionSeverityWarning, "")
		conditionList := hetznerCluster.GetConditions()
		conditionList[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Hour))
		Expect(reconcileRateLimit(hetznerCluster)).To(BeFalse())
	})

	It("returns wait== false if rate limit condition is set to true", func() {
		conditions.MarkTrue(hetznerCluster, infrav1.HetznerAPIReachableCondition)
		Expect(reconcileRateLimit(hetznerCluster)).To(BeFalse())
	})

	It("returns wait== false if rate limit condition is not set", func() {
		Expect(reconcileRateLimit(hetznerCluster)).To(BeFalse())
	})
})
