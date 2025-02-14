/*
Copyright 2015 The Kubernetes Authors.

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

package podgc

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	metricstestutil "k8s.io/component-base/metrics/testutil"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/testutil"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	testingclock "k8s.io/utils/clock/testing"
)

func alwaysReady() bool { return true }

func NewFromClient(ctx context.Context, kubeClient clientset.Interface, terminatedPodThreshold int) (*PodGCController, coreinformers.PodInformer, coreinformers.NodeInformer) {
	informerFactory := informers.NewSharedInformerFactory(kubeClient, controller.NoResyncPeriodFunc())
	podInformer := informerFactory.Core().V1().Pods()
	nodeInformer := informerFactory.Core().V1().Nodes()
	controller := NewPodGC(ctx, kubeClient, podInformer, nodeInformer, terminatedPodThreshold)
	controller.podListerSynced = alwaysReady
	return controller, podInformer, nodeInformer
}

func TestGCTerminated(t *testing.T) {
	type nameToPhase struct {
		name   string
		phase  v1.PodPhase
		reason string
	}

	testCases := []struct {
		name                          string
		pods                          []nameToPhase
		threshold                     int
		deletedPodNames               sets.String
		patchedPodNames               sets.String
		enablePodDisruptionConditions bool
	}{
		{
			name: "delete pod a which is PodFailed and pod b which is PodSucceeded; PodDisruptionConditions enabled",
			pods: []nameToPhase{
				{name: "a", phase: v1.PodFailed},
				{name: "b", phase: v1.PodSucceeded},
				{name: "c", phase: v1.PodFailed},
			},
			threshold:                     1,
			patchedPodNames:               sets.NewString(),
			deletedPodNames:               sets.NewString("a", "b"),
			enablePodDisruptionConditions: true,
		},
		{
			name: "threshold = 0, disables terminated pod deletion",
			pods: []nameToPhase{
				{name: "a", phase: v1.PodFailed},
				{name: "b", phase: v1.PodSucceeded},
			},
			threshold: 0,
			// threshold = 0 disables terminated pod deletion
			deletedPodNames: sets.NewString(),
		},
		{
			name: "threshold = 1, delete pod a which is PodFailed and pod b which is PodSucceeded",
			pods: []nameToPhase{
				{name: "a", phase: v1.PodFailed},
				{name: "b", phase: v1.PodSucceeded},
				{name: "c", phase: v1.PodFailed},
			},
			threshold:       1,
			deletedPodNames: sets.NewString("a", "b"),
		},
		{
			name: "threshold = 1, delete pod b which is PodSucceeded",
			pods: []nameToPhase{
				{name: "a", phase: v1.PodRunning},
				{name: "b", phase: v1.PodSucceeded},
				{name: "c", phase: v1.PodFailed},
			},
			threshold:       1,
			deletedPodNames: sets.NewString("b"),
		},
		{
			name: "threshold = 1, delete pod a which is PodFailed",
			pods: []nameToPhase{
				{name: "a", phase: v1.PodFailed},
				{name: "b", phase: v1.PodSucceeded},
			},
			threshold:       1,
			deletedPodNames: sets.NewString("a"),
		},
		{
			name: "threshold = 5, don't delete pod",
			pods: []nameToPhase{
				{name: "a", phase: v1.PodFailed},
				{name: "b", phase: v1.PodSucceeded},
			},
			threshold:       5,
			deletedPodNames: sets.NewString(),
		},
		{
			pods: []nameToPhase{
				{name: "a", phase: v1.PodFailed},
				{name: "b", phase: v1.PodSucceeded},
				{name: "c", phase: v1.PodFailed, reason: eviction.Reason},
			},
			threshold:       1,
			deletedPodNames: sets.NewString("c", "a"),
		},
		{
			pods: []nameToPhase{
				{name: "a", phase: v1.PodRunning},
				{name: "b", phase: v1.PodSucceeded},
				{name: "c", phase: v1.PodFailed, reason: eviction.Reason},
			},
			threshold:       1,
			deletedPodNames: sets.NewString("c"),
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodDisruptionConditions, test.enablePodDisruptionConditions)()
			creationTime := time.Unix(0, 0)
			nodes := []*v1.Node{testutil.NewNode("node")}

			pods := make([]*v1.Pod, 0, len(test.pods))
			for _, pod := range test.pods {
				creationTime = creationTime.Add(1 * time.Hour)
				pods = append(pods, &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: pod.name, CreationTimestamp: metav1.Time{Time: creationTime}},
					Status:     v1.PodStatus{Phase: pod.phase, Reason: pod.reason},
					Spec:       v1.PodSpec{NodeName: "node"},
				})
			}
			client := setupNewSimpleClient(nodes, pods)
			gcc, podInformer, _ := NewFromClient(ctx, client, test.threshold)
			for _, pod := range pods {
				podInformer.Informer().GetStore().Add(pod)
			}

			gcc.gc(ctx)

			verifyDeletedAndPatchedPods(t, client, test.deletedPodNames, test.patchedPodNames)
		})
	}
}

func makePod(name string, nodeName string, phase v1.PodPhase) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec:   v1.PodSpec{NodeName: nodeName},
		Status: v1.PodStatus{Phase: phase},
	}
}

func waitForAdded(q workqueue.DelayingInterface, depth int) error {
	return wait.Poll(1*time.Millisecond, 10*time.Second, func() (done bool, err error) {
		if q.Len() == depth {
			return true, nil
		}

		return false, nil
	})
}

func TestGCOrphaned(t *testing.T) {
	testCases := []struct {
		name                          string
		initialClientNodes            []*v1.Node
		initialInformerNodes          []*v1.Node
		delay                         time.Duration
		addedClientNodes              []*v1.Node
		deletedClientNodes            []*v1.Node
		addedInformerNodes            []*v1.Node
		deletedInformerNodes          []*v1.Node
		pods                          []*v1.Pod
		itemsInQueue                  int
		deletedPodNames               sets.String
		patchedPodNames               sets.String
		enablePodDisruptionConditions bool
	}{
		{
			name: "nodes present in lister",
			initialInformerNodes: []*v1.Node{
				testutil.NewNode("existing1"),
				testutil.NewNode("existing2"),
			},
			delay: 2 * quarantineTime,
			pods: []*v1.Pod{
				makePod("a", "existing1", v1.PodRunning),
				makePod("b", "existing2", v1.PodFailed),
				makePod("c", "existing2", v1.PodSucceeded),
			},
			itemsInQueue:    0,
			deletedPodNames: sets.NewString(),
		},
		{
			name: "nodes present in client",
			initialClientNodes: []*v1.Node{
				testutil.NewNode("existing1"),
				testutil.NewNode("existing2"),
			},
			delay: 2 * quarantineTime,
			pods: []*v1.Pod{
				makePod("a", "existing1", v1.PodRunning),
				makePod("b", "existing2", v1.PodFailed),
				makePod("c", "existing2", v1.PodSucceeded),
			},
			itemsInQueue:    2,
			deletedPodNames: sets.NewString(),
		},
		{
			name:  "no nodes",
			delay: 2 * quarantineTime,
			pods: []*v1.Pod{
				makePod("a", "deleted", v1.PodFailed),
				makePod("b", "deleted", v1.PodSucceeded),
			},
			itemsInQueue:    1,
			deletedPodNames: sets.NewString("a", "b"),
		},
		{
			name:  "no nodes with PodDisruptionConditions enabled",
			delay: 2 * quarantineTime,
			pods: []*v1.Pod{
				makePod("a", "deleted", v1.PodFailed),
				makePod("b", "deleted", v1.PodSucceeded),
				makePod("c", "deleted", v1.PodRunning),
			},
			itemsInQueue:                  1,
			deletedPodNames:               sets.NewString("a", "b", "c"),
			patchedPodNames:               sets.NewString("c"),
			enablePodDisruptionConditions: true,
		},
		{
			name:  "quarantine not finished",
			delay: quarantineTime / 2,
			pods: []*v1.Pod{
				makePod("a", "deleted", v1.PodFailed),
			},
			itemsInQueue:    0,
			deletedPodNames: sets.NewString(),
		},
		{
			name:                 "wrong nodes",
			initialInformerNodes: []*v1.Node{testutil.NewNode("existing")},
			delay:                2 * quarantineTime,
			pods: []*v1.Pod{
				makePod("a", "deleted", v1.PodRunning),
			},
			itemsInQueue:    1,
			deletedPodNames: sets.NewString("a"),
		},
		{
			name:                 "some nodes missing",
			initialInformerNodes: []*v1.Node{testutil.NewNode("existing")},
			delay:                2 * quarantineTime,
			pods: []*v1.Pod{
				makePod("a", "deleted", v1.PodFailed),
				makePod("b", "existing", v1.PodFailed),
				makePod("c", "deleted", v1.PodSucceeded),
				makePod("d", "deleted", v1.PodRunning),
			},
			itemsInQueue:    1,
			deletedPodNames: sets.NewString("a", "c", "d"),
		},
		{
			name:             "node added to client after quarantine",
			delay:            2 * quarantineTime,
			addedClientNodes: []*v1.Node{testutil.NewNode("node")},
			pods: []*v1.Pod{
				makePod("a", "node", v1.PodRunning),
			},
			itemsInQueue:    1,
			deletedPodNames: sets.NewString(),
		},
		{
			name:               "node added to informer after quarantine",
			delay:              2 * quarantineTime,
			addedInformerNodes: []*v1.Node{testutil.NewNode("node")},
			pods: []*v1.Pod{
				makePod("a", "node", v1.PodFailed),
			},
			itemsInQueue:    1,
			deletedPodNames: sets.NewString(),
		},
		{
			// It shouldn't happen that client will be lagging behind informer.
			// This test case is more a sanity check.
			name:               "node deleted from client after quarantine",
			initialClientNodes: []*v1.Node{testutil.NewNode("node")},
			delay:              2 * quarantineTime,
			deletedClientNodes: []*v1.Node{testutil.NewNode("node")},
			pods: []*v1.Pod{
				makePod("a", "node", v1.PodFailed),
			},
			itemsInQueue:    1,
			deletedPodNames: sets.NewString("a"),
		},
		{
			name:                 "node deleted from informer after quarantine",
			initialInformerNodes: []*v1.Node{testutil.NewNode("node")},
			delay:                2 * quarantineTime,
			deletedInformerNodes: []*v1.Node{testutil.NewNode("node")},
			pods: []*v1.Pod{
				makePod("a", "node", v1.PodSucceeded),
			},
			itemsInQueue:    0,
			deletedPodNames: sets.NewString(),
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodDisruptionConditions, test.enablePodDisruptionConditions)()
			nodes := make([]*v1.Node, 0, len(test.initialClientNodes))
			for _, node := range test.initialClientNodes {
				nodes = append(nodes, node)
			}
			pods := make([]*v1.Pod, 0, len(test.pods))
			for _, pod := range test.pods {
				pods = append(pods, pod)
			}
			client := setupNewSimpleClient(nodes, pods)
			gcc, podInformer, nodeInformer := NewFromClient(ctx, client, -1)
			for _, node := range test.initialInformerNodes {
				nodeInformer.Informer().GetStore().Add(node)
			}
			for _, pod := range test.pods {
				podInformer.Informer().GetStore().Add(pod)
			}
			// Overwrite queue
			fakeClock := testingclock.NewFakeClock(time.Now())
			gcc.nodeQueue.ShutDown()
			gcc.nodeQueue = workqueue.NewDelayingQueueWithCustomClock(fakeClock, "podgc_test_queue")

			// First GC of orphaned pods
			gcc.gc(ctx)
			deletedPodNames := getDeletedPodNames(client)

			if len(deletedPodNames) > 0 {
				t.Errorf("no pods should be deleted at this point.\n\tactual: %v", deletedPodNames)
			}

			// Move clock forward
			fakeClock.Step(test.delay)
			// Wait for queue goroutine to process items
			if test.itemsInQueue > 0 {
				err := waitForAdded(gcc.nodeQueue, test.itemsInQueue)
				if err != nil {
					t.Errorf("wrong number of items in the node queue.\n\texpected: %v\n\tactual: %v",
						test.itemsInQueue, gcc.nodeQueue.Len())
				}
			}

			// Execute planned nodes changes
			for _, node := range test.addedClientNodes {
				client.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
			}
			for _, node := range test.deletedClientNodes {
				client.CoreV1().Nodes().Delete(context.TODO(), node.Name, metav1.DeleteOptions{})
			}
			for _, node := range test.addedInformerNodes {
				nodeInformer.Informer().GetStore().Add(node)
			}
			for _, node := range test.deletedInformerNodes {
				nodeInformer.Informer().GetStore().Delete(node)
			}

			// Actual pod deletion
			gcc.gc(context.TODO())
			verifyDeletedAndPatchedPods(t, client, test.deletedPodNames, test.patchedPodNames)
		})
	}
}

func TestGCUnscheduledTerminating(t *testing.T) {
	type nameToPhase struct {
		name              string
		phase             v1.PodPhase
		deletionTimeStamp *metav1.Time
		nodeName          string
	}

	testCases := []struct {
		name                          string
		pods                          []nameToPhase
		deletedPodNames               sets.String
		patchedPodNames               sets.String
		enablePodDisruptionConditions bool
	}{
		{
			name: "Unscheduled pod in any phase must be deleted, the phase of the running pod is changed to Failed; PodDisruptionConditions enabled",
			pods: []nameToPhase{
				{name: "a", phase: v1.PodFailed, deletionTimeStamp: &metav1.Time{}, nodeName: ""},
				{name: "b", phase: v1.PodSucceeded, deletionTimeStamp: &metav1.Time{}, nodeName: ""},
				{name: "c", phase: v1.PodRunning, deletionTimeStamp: &metav1.Time{}, nodeName: ""},
			},
			deletedPodNames:               sets.NewString("a", "b", "c"),
			patchedPodNames:               sets.NewString("c"),
			enablePodDisruptionConditions: true,
		},
		{
			name: "Unscheduled pod in any phase must be deleted",
			pods: []nameToPhase{
				{name: "a", phase: v1.PodFailed, deletionTimeStamp: &metav1.Time{}, nodeName: ""},
				{name: "b", phase: v1.PodSucceeded, deletionTimeStamp: &metav1.Time{}, nodeName: ""},
				{name: "c", phase: v1.PodRunning, deletionTimeStamp: &metav1.Time{}, nodeName: ""},
			},
			deletedPodNames: sets.NewString("a", "b", "c"),
		},
		{
			name: "Scheduled pod in any phase must not be deleted",
			pods: []nameToPhase{
				{name: "a", phase: v1.PodFailed, deletionTimeStamp: nil, nodeName: ""},
				{name: "b", phase: v1.PodSucceeded, deletionTimeStamp: nil, nodeName: "node"},
				{name: "c", phase: v1.PodRunning, deletionTimeStamp: &metav1.Time{}, nodeName: "node"},
			},
			deletedPodNames: sets.NewString(),
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodDisruptionConditions, test.enablePodDisruptionConditions)()
			creationTime := time.Unix(0, 0)

			pods := make([]*v1.Pod, 0, len(test.pods))
			for _, pod := range test.pods {
				creationTime = creationTime.Add(1 * time.Hour)
				pods = append(pods, &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: pod.name, CreationTimestamp: metav1.Time{Time: creationTime},
						DeletionTimestamp: pod.deletionTimeStamp},
					Status: v1.PodStatus{Phase: pod.phase},
					Spec:   v1.PodSpec{NodeName: pod.nodeName},
				})
			}
			nodes := []*v1.Node{}
			client := setupNewSimpleClient(nodes, pods)
			gcc, podInformer, _ := NewFromClient(ctx, client, -1)

			for _, pod := range pods {
				podInformer.Informer().GetStore().Add(pod)
			}

			pods, err := podInformer.Lister().List(labels.Everything())
			if err != nil {
				t.Errorf("Error while listing all Pods: %v", err)
				return
			}
			gcc.gcUnscheduledTerminating(ctx, pods)
			verifyDeletedAndPatchedPods(t, client, test.deletedPodNames, test.patchedPodNames)
		})
	}
}

func TestGCTerminating(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeOutOfServiceVolumeDetach, true)()
	type node struct {
		name           string
		readyCondition v1.ConditionStatus
		taints         []v1.Taint
	}

	type nameToPodConfig struct {
		name              string
		phase             v1.PodPhase
		deletionTimeStamp *metav1.Time
		nodeName          string
	}

	testCases := []struct {
		name                          string
		pods                          []nameToPodConfig
		nodes                         []node
		deletedPodNames               sets.String
		patchedPodNames               sets.String
		enablePodDisruptionConditions bool
	}{
		{
			name: "pods have deletion timestamp set and the corresponding nodes are not ready",
			nodes: []node{
				{name: "worker-0", readyCondition: v1.ConditionFalse},
				{name: "worker-1", readyCondition: v1.ConditionFalse},
			},
			pods: []nameToPodConfig{
				{name: "a", deletionTimeStamp: &metav1.Time{}, nodeName: "worker-0"},
				{name: "b", deletionTimeStamp: &metav1.Time{}, nodeName: "worker-1"},
			},
			deletedPodNames: sets.NewString(),
		},

		{
			name: "some pods have deletion timestamp and/or phase set and some of the corresponding nodes have an" +
				"outofservice taint that are not ready",
			nodes: []node{
				// terminated pods on this node should be force deleted
				{name: "worker-0", readyCondition: v1.ConditionFalse, taints: []v1.Taint{{Key: v1.TaintNodeOutOfService,
					Effect: v1.TaintEffectNoExecute}}},
				// terminated pods on this node should not be force deleted
				{name: "worker-1", readyCondition: v1.ConditionFalse},
				// terminated pods on this node should not be force deleted
				{name: "worker-2", readyCondition: v1.ConditionTrue},
				// terminated pods on this node should be force deleted
				{name: "worker-3", readyCondition: v1.ConditionFalse, taints: []v1.Taint{{Key: v1.TaintNodeOutOfService,
					Effect: v1.TaintEffectNoSchedule}}},
				// terminated pods on this node should be force deleted
				{name: "worker-4", readyCondition: v1.ConditionFalse, taints: []v1.Taint{{Key: v1.TaintNodeOutOfService,
					Effect: v1.TaintEffectPreferNoSchedule}}},
				// terminated pods on this node should be force deleted
				{name: "worker-5", readyCondition: v1.ConditionFalse, taints: []v1.Taint{{Key: v1.TaintNodeOutOfService,
					Value: "any-value", Effect: v1.TaintEffectNoExecute}}},
			},
			pods: []nameToPodConfig{
				// pods a1, b1, c1, d1 and e1 are on node worker-0
				{name: "a1", nodeName: "worker-0"},
				{name: "b1", deletionTimeStamp: &metav1.Time{}, nodeName: "worker-0"},
				{name: "c1", phase: v1.PodPending, nodeName: "worker-0"},
				{name: "d1", phase: v1.PodRunning, nodeName: "worker-0"},
				{name: "e1", phase: v1.PodUnknown, nodeName: "worker-0"},

				// pods a2, b2, c2, d2 and e2 are on node worker-1
				{name: "a2", nodeName: "worker-1"},
				{name: "b2", deletionTimeStamp: &metav1.Time{}, nodeName: "worker-1"},
				{name: "c2", phase: v1.PodPending, nodeName: "worker-1"},
				{name: "d2", phase: v1.PodRunning, nodeName: "worker-1"},
				{name: "e2", phase: v1.PodUnknown, nodeName: "worker-1"},

				// pods a3, b3, c3, d3 and e3 are on node worker-2
				{name: "a3", nodeName: "worker-2"},
				{name: "b3", deletionTimeStamp: &metav1.Time{}, nodeName: "worker-2"},
				{name: "c3", phase: v1.PodPending, nodeName: "worker-2"},
				{name: "d3", phase: v1.PodRunning, nodeName: "worker-2"},
				{name: "e3", phase: v1.PodUnknown, nodeName: "worker-2"},

				// pods a4, b4, c4, d4 and e4 are on node worker-3
				{name: "a4", nodeName: "worker-3"},
				{name: "b4", deletionTimeStamp: &metav1.Time{}, nodeName: "worker-3"},
				{name: "c4", phase: v1.PodPending, nodeName: "worker-3"},
				{name: "d4", phase: v1.PodRunning, nodeName: "worker-3"},
				{name: "e4", phase: v1.PodUnknown, nodeName: "worker-3"},

				// pods a5, b5, c5, d5 and e5 are on node worker-4
				{name: "a5", nodeName: "worker-3"},
				{name: "b5", deletionTimeStamp: &metav1.Time{}, nodeName: "worker-4"},
				{name: "c5", phase: v1.PodPending, nodeName: "worker-4"},
				{name: "d5", phase: v1.PodRunning, nodeName: "worker-4"},
				{name: "e5", phase: v1.PodUnknown, nodeName: "worker-4"},

				// pods a6, b6, c6, d6 and e6 are on node worker-5
				{name: "a6", nodeName: "worker-5"},
				{name: "b6", deletionTimeStamp: &metav1.Time{}, nodeName: "worker-5"},
				{name: "c6", phase: v1.PodPending, nodeName: "worker-5"},
				{name: "d6", phase: v1.PodRunning, nodeName: "worker-5"},
				{name: "e6", phase: v1.PodUnknown, nodeName: "worker-5"},
			},
			deletedPodNames: sets.NewString("b1", "b4", "b5", "b6"),
		},
		{
			name: "pods deleted from node tained out-of-service; PodDisruptionConditions enabled",
			nodes: []node{
				{name: "worker", readyCondition: v1.ConditionFalse, taints: []v1.Taint{{Key: v1.TaintNodeOutOfService,
					Effect: v1.TaintEffectNoExecute}}},
			},
			pods: []nameToPodConfig{
				{name: "a", phase: v1.PodRunning, deletionTimeStamp: &metav1.Time{}, nodeName: "worker"},
				{name: "b", phase: v1.PodFailed, deletionTimeStamp: &metav1.Time{}, nodeName: "worker"},
				{name: "c", phase: v1.PodSucceeded, deletionTimeStamp: &metav1.Time{}, nodeName: "worker"},
			},
			deletedPodNames:               sets.NewString("a", "b", "c"),
			patchedPodNames:               sets.NewString("a"),
			enablePodDisruptionConditions: true,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodDisruptionConditions, test.enablePodDisruptionConditions)()

			creationTime := time.Unix(0, 0)
			nodes := make([]*v1.Node, 0, len(test.nodes))
			for _, node := range test.nodes {
				creationTime = creationTime.Add(2 * time.Hour)
				nodes = append(nodes, &v1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: node.name, CreationTimestamp: metav1.Time{Time: creationTime}},
					Spec: v1.NodeSpec{
						Taints: node.taints,
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: node.readyCondition,
							},
						},
					},
				})
			}
			pods := make([]*v1.Pod, 0, len(test.pods))
			for _, pod := range test.pods {
				creationTime = creationTime.Add(1 * time.Hour)
				pods = append(pods, &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: pod.name, CreationTimestamp: metav1.Time{Time: creationTime},
						DeletionTimestamp: pod.deletionTimeStamp},
					Status: v1.PodStatus{Phase: pod.phase},
					Spec:   v1.PodSpec{NodeName: pod.nodeName},
				})
			}
			client := setupNewSimpleClient(nodes, pods)
			gcc, podInformer, nodeInformer := NewFromClient(ctx, client, -1)

			for _, pod := range pods {
				podInformer.Informer().GetStore().Add(pod)
			}
			for _, node := range nodes {
				nodeInformer.Informer().GetStore().Add(node)
			}

			gcc.gc(ctx)
			verifyDeletedAndPatchedPods(t, client, test.deletedPodNames, test.patchedPodNames)
		})
	}
	// deletingPodsTotal is 7 in this test
	testDeletingPodsMetrics(t, 7)
}

func verifyDeletedAndPatchedPods(t *testing.T, client *fake.Clientset, wantDeletedPodNames, wantPatchedPodNames sets.String) {
	t.Helper()
	deletedPodNames := getDeletedPodNames(client)
	if diff := cmp.Diff(wantDeletedPodNames, deletedPodNames); diff != "" {
		t.Errorf("Deleted pod names (-want,+got):\n%s", diff)
	}
	patchedPodNames := getPatchedPodNames(client)
	if diff := cmp.Diff(wantPatchedPodNames, patchedPodNames); diff != "" {
		t.Errorf("Patched pod names (-want,+got):\n%s", diff)
	}
}

func testDeletingPodsMetrics(t *testing.T, inputDeletingPodsTotal int) {
	t.Helper()

	actualDeletingPodsTotal, err := metricstestutil.GetCounterMetricValue(deletingPodsTotal.WithLabelValues())
	if err != nil {
		t.Errorf("Error getting actualDeletingPodsTotal")
	}
	if actualDeletingPodsTotal != float64(inputDeletingPodsTotal) {
		t.Errorf("Expected desiredDeletingPodsTotal to be %d, got %v", inputDeletingPodsTotal, actualDeletingPodsTotal)
	}

	actualDeletingPodsErrorTotal, err := metricstestutil.GetCounterMetricValue(deletingPodsErrorTotal.WithLabelValues())
	if err != nil {
		t.Errorf("Error getting actualDeletingPodsErrorTotal")
	}
	if actualDeletingPodsErrorTotal != float64(0) {
		t.Errorf("Expected desiredDeletingPodsTotal to be %d, got %v", 0, actualDeletingPodsErrorTotal)
	}
}

func setupNewSimpleClient(nodes []*v1.Node, pods []*v1.Pod) *fake.Clientset {
	podList := &v1.PodList{}
	for _, podItem := range pods {
		podList.Items = append(podList.Items, *podItem)
	}
	nodeList := &v1.NodeList{}
	for _, nodeItem := range nodes {
		nodeList.Items = append(nodeList.Items, *nodeItem)
	}
	return fake.NewSimpleClientset(nodeList, podList)
}

func getDeletedPodNames(client *fake.Clientset) sets.String {
	deletedPodNames := sets.NewString()
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "pods" {
			deleteAction := action.(clienttesting.DeleteAction)
			deletedPodNames.Insert(deleteAction.GetName())
		}
	}
	return deletedPodNames
}

func getPatchedPodNames(client *fake.Clientset) sets.String {
	patchedPodNames := sets.NewString()
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "pods" {
			patchAction := action.(clienttesting.PatchAction)
			patchedPodNames.Insert(patchAction.GetName())
		}
	}
	return patchedPodNames
}
