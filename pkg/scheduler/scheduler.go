/*
Copyright 2024 The HAMi Authors.

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

package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/k8sutil"
	"github.com/Project-HAMi/HAMi/pkg/scheduler/config"
	"github.com/Project-HAMi/HAMi/pkg/scheduler/policy"
	"github.com/Project-HAMi/HAMi/pkg/util"
)

type Scheduler struct {
	nodeManager
	podManager

	stopCh     chan struct{}
	kubeClient kubernetes.Interface
	podLister  listerscorev1.PodLister
	nodeLister listerscorev1.NodeLister
	//Node status returned by filter
	cachedstatus map[string]*NodeUsage
	nodeNotify   chan struct{}
	//Node Overview
	overviewstatus map[string]*NodeUsage

	eventRecorder record.EventRecorder
}

func NewScheduler() *Scheduler {
	klog.Info("New Scheduler")
	s := &Scheduler{
		stopCh:       make(chan struct{}),
		cachedstatus: make(map[string]*NodeUsage),
		nodeNotify:   make(chan struct{}, 1),
	}
	s.nodeManager.init()
	s.podManager.init()
	return s
}

func check(err error) {
	if err != nil {
		klog.Fatal(err)
	}
}

func (s *Scheduler) onUpdateNode(_, newObj interface{}) {
	s.nodeNotify <- struct{}{}
}

func (s *Scheduler) onDelNode(obj interface{}) {
	s.nodeNotify <- struct{}{}
}

func (s *Scheduler) onAddNode(obj interface{}) {
	s.nodeNotify <- struct{}{}
}

func (s *Scheduler) onAddPod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		klog.Errorf("unknown add object type")
		return
	}
	nodeID, ok := pod.Annotations[util.AssignedNodeAnnotations]
	if !ok {
		return
	}
	if k8sutil.IsPodInTerminatedState(pod) {
		s.delPod(pod)
		return
	}
	podDev, _ := util.DecodePodDevices(util.SupportDevices, pod.Annotations)
	s.addPod(pod, nodeID, podDev)
}

func (s *Scheduler) onUpdatePod(_, newObj interface{}) {
	s.onAddPod(newObj)
}

func (s *Scheduler) onDelPod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		klog.Errorf("unknown add object type")
		return
	}
	_, ok = pod.Annotations[util.AssignedNodeAnnotations]
	if !ok {
		return
	}
	s.delPod(pod)
}

func (s *Scheduler) Start() {
	kubeClient, err := k8sutil.NewClient()
	check(err)
	s.kubeClient = kubeClient
	informerFactory := informers.NewSharedInformerFactoryWithOptions(s.kubeClient, time.Hour*1)
	s.podLister = informerFactory.Core().V1().Pods().Lister()
	s.nodeLister = informerFactory.Core().V1().Nodes().Lister()

	informer := informerFactory.Core().V1().Pods().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    s.onAddPod,
		UpdateFunc: s.onUpdatePod,
		DeleteFunc: s.onDelPod,
	})
	informerFactory.Core().V1().Nodes().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    s.onAddNode,
		UpdateFunc: s.onUpdateNode,
		DeleteFunc: s.onDelNode,
	})
	informerFactory.Start(s.stopCh)
	informerFactory.WaitForCacheSync(s.stopCh)
	s.addAllEventHandlers()
}

func (s *Scheduler) Stop() {
	close(s.stopCh)
}

func (s *Scheduler) RegisterFromNodeAnnotations() {
	klog.V(5).Infoln("Scheduler into RegisterFromNodeAnnotations")
	ticker := time.NewTicker(time.Second * 15)
	printedLog := map[string]bool{}
	for {
		select {
		case <-s.nodeNotify:
		case <-ticker.C:
		case <-s.stopCh:
			return
		}
		labelSelector := labels.Everything()
		if len(config.NodeLabelSelector) > 0 {
			labelSelector = (labels.Set)(config.NodeLabelSelector).AsSelector()
		}
		rawNodes, err := s.nodeLister.List(labelSelector)
		if err != nil {
			klog.Errorln("nodes list failed", err.Error())
			continue
		}
		var nodeNames []string
		for _, val := range rawNodes {
			nodeNames = append(nodeNames, val.Name)
			for devhandsk, devInstance := range device.GetDevices() {
				health, needUpdate := devInstance.CheckHealth(devhandsk, val)
				klog.V(5).InfoS("device check health", "node", val.Name, "deviceVendor", devhandsk, "health", health, "needUpdate", needUpdate)
				if !health {
					err := devInstance.NodeCleanUp(val.Name)
					// If the device is not healthy, the device is removed from the node.
					// At the same time, this node needs to be removed from the cache.
					if err != nil {
						klog.Errorln("node cleanup failed", err.Error())
					}
					info, ok := s.nodes[val.Name]
					if ok {
						klog.Infof("node %v device %s:%v leave, %v remaining devices:%v", val.Name, devhandsk, info.ID, err, s.nodes[val.Name].Devices)
						s.rmNodeDevice(val.Name, info, devhandsk)
						continue
					}
				}
				if !needUpdate {
					continue
				}
				_, ok := util.HandshakeAnnos[devhandsk]
				if ok {
					tmppat := make(map[string]string)
					tmppat[util.HandshakeAnnos[devhandsk]] = "Requesting_" + time.Now().Format(time.DateTime)
					klog.V(5).InfoS("New timestamp", util.HandshakeAnnos[devhandsk], tmppat[util.HandshakeAnnos[devhandsk]], "nodeName", val.Name)
					n, err := util.GetNode(val.Name)
					if err != nil {
						klog.Errorln("get node failed", err.Error())
						continue
					}
					util.PatchNodeAnnotations(n, tmppat)
				}

				nodeInfo := &util.NodeInfo{}
				nodeInfo.ID = val.Name
				nodeInfo.Node = val
				nodedevices, err := devInstance.GetNodeDevices(*val)
				if err != nil {
					continue
				}
				nodeInfo.Devices = make([]util.DeviceInfo, 0)
				for _, deviceinfo := range nodedevices {
					nodeInfo.Devices = append(nodeInfo.Devices, *deviceinfo)
				}
				s.addNode(val.Name, nodeInfo)
				if s.nodes[val.Name] != nil && len(nodeInfo.Devices) > 0 {
					if printedLog[val.Name] {
						klog.Infof("node %v device %s come node info=%s,%v total=%v", val.Name, devhandsk, nodeInfo.ID, nodeInfo.Devices, s.nodes[val.Name].Devices)
						printedLog[val.Name] = true
					} else {
						klog.V(5).Infof("node %v device %s come node info=%s,%v total=%v", val.Name, devhandsk, nodeInfo.ID, nodeInfo.Devices, s.nodes[val.Name].Devices)
					}
				}
			}
		}
		_, _, err = s.getNodesUsage(&nodeNames, nil)
		if err != nil {
			klog.Errorln("get node usage failed", err.Error())
		}
	}
}

// InspectAllNodesUsage is used by metrics monitor.
func (s *Scheduler) InspectAllNodesUsage() *map[string]*NodeUsage {
	return &s.overviewstatus
}

// returns all nodes and its device memory usage, and we filter it with nodeSelector, taints, nodeAffinity
// unschedulerable and nodeName.
func (s *Scheduler) getNodesUsage(nodes *[]string, task *corev1.Pod) (*map[string]*NodeUsage, map[string]string, error) {
	overallnodeMap := make(map[string]*NodeUsage)
	cachenodeMap := make(map[string]*NodeUsage)
	failedNodes := make(map[string]string)
	//for _, nodeID := range *nodes {
	allNodes, err := s.ListNodes()
	if err != nil {
		return &overallnodeMap, failedNodes, err
	}

	for _, node := range allNodes {
		nodeInfo := &NodeUsage{}
		userGPUPolicy := config.GPUSchedulerPolicy
		if task != nil && task.Annotations != nil {
			if value, ok := task.Annotations[policy.GPUSchedulerPolicyAnnotationKey]; ok {
				userGPUPolicy = value
			}
		}
		nodeInfo.Node = node.Node
		nodeInfo.Devices = policy.DeviceUsageList{
			Policy:      userGPUPolicy,
			DeviceLists: make([]*policy.DeviceListsScore, 0),
		}
		for _, d := range node.Devices {
			nodeInfo.Devices.DeviceLists = append(nodeInfo.Devices.DeviceLists, &policy.DeviceListsScore{
				Score: 0,
				Device: &util.DeviceUsage{
					ID:        d.ID,
					Index:     d.Index,
					Used:      0,
					Count:     d.Count,
					Usedmem:   0,
					Totalmem:  d.Devmem,
					Totalcore: d.Devcore,
					Usedcores: 0,
					MigUsage: util.MigInUse{
						Index:     0,
						UsageList: make(util.MIGS, 0),
					},
					MigTemplate: d.MIGTemplate,
					Mode:        d.Mode,
					Type:        d.Type,
					Numa:        d.Numa,
					Health:      d.Health,
				},
			})
		}
		overallnodeMap[node.ID] = nodeInfo
	}

	podsInfo := s.ListPodsInfo()
	for _, p := range podsInfo {
		node, ok := overallnodeMap[p.NodeID]
		if !ok {
			continue
		}
		for _, podsingleds := range p.Devices {
			for _, ctrdevs := range podsingleds {
				for _, udevice := range ctrdevs {
					for _, d := range node.Devices.DeviceLists {
						deviceID := udevice.UUID
						if strings.Contains(deviceID, "[") {
							deviceID = strings.Split(deviceID, "[")[0]
						}
						if d.Device.ID == deviceID {
							d.Device.Used++
							d.Device.Usedmem += udevice.Usedmem
							d.Device.Usedcores += udevice.Usedcores
							if strings.Contains(udevice.UUID, "[") {
								tmpIdx, Instance := util.ExtractMigTemplatesFromUUID(udevice.UUID)
								if len(d.Device.MigUsage.UsageList) == 0 {
									util.PlatternMIG(&d.Device.MigUsage, d.Device.MigTemplate, tmpIdx)
								}
								d.Device.MigUsage.UsageList[Instance].InUse = true
								klog.V(5).Infoln("add mig usage", d.Device.MigUsage, "template=", d.Device.MigTemplate, "uuid=", d.Device.ID)
							}
						}
					}
				}
			}
		}
		klog.V(5).Infof("usage: pod %v assigned %v %v", p.Name, p.NodeID, p.Devices)
	}
	s.overviewstatus = overallnodeMap
	for _, nodeID := range *nodes {
		node, err := s.GetNode(nodeID)
		if err != nil {
			// The identified node does not have a gpu device, so the log here has no practical meaning,increase log priority.
			klog.V(5).InfoS("node unregistered", "node", nodeID, "error", err)
			failedNodes[nodeID] = "node unregistered"
			continue
		}
		cachenodeMap[node.ID] = overallnodeMap[node.ID]
	}
	s.cachedstatus = cachenodeMap
	return &cachenodeMap, failedNodes, nil
}

func (s *Scheduler) getPodUsage() (map[string]PodUseDeviceStat, error) {
	podUsageStat := make(map[string]PodUseDeviceStat)
	pods, err := s.podLister.List(labels.NewSelector())
	if err != nil {
		return nil, err
	}
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodSucceeded {
			continue
		}
		podUseDeviceNum := 0
		if v, ok := pod.Annotations[util.DeviceBindPhase]; ok && v == util.DeviceBindSuccess {
			podUseDeviceNum = 1
		}
		nodeName := pod.Spec.NodeName
		if _, ok := podUsageStat[nodeName]; !ok {
			podUsageStat[nodeName] = PodUseDeviceStat{
				TotalPod:     1,
				UseDevicePod: podUseDeviceNum,
			}
		} else {
			exist := podUsageStat[nodeName]
			podUsageStat[nodeName] = PodUseDeviceStat{
				TotalPod:     exist.TotalPod + 1,
				UseDevicePod: exist.UseDevicePod + podUseDeviceNum,
			}
		}
	}
	return podUsageStat, nil
}

func (s *Scheduler) Bind(args extenderv1.ExtenderBindingArgs) (*extenderv1.ExtenderBindingResult, error) {
	klog.InfoS("Bind", "pod", args.PodName, "namespace", args.PodNamespace, "podUID", args.PodUID, "node", args.Node)
	var err error
	var res *extenderv1.ExtenderBindingResult
	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: args.PodName, UID: args.PodUID},
		Target:     corev1.ObjectReference{Kind: "Node", Name: args.Node},
	}
	current, err := s.kubeClient.CoreV1().Pods(args.PodNamespace).Get(context.Background(), args.PodName, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "Get pod failed")
	}

	node, err := s.kubeClient.CoreV1().Nodes().Get(context.Background(), args.Node, metav1.GetOptions{})
	if err != nil {
		klog.ErrorS(err, "Failed to get node", "node", args.Node)
		s.recordScheduleBindingResultEvent(current, EventReasonBindingFailed, []string{}, fmt.Errorf("failed to get node %v", args.Node))
		res = &extenderv1.ExtenderBindingResult{
			Error: err.Error(),
		}
		return res, nil
	}

	tmppatch := make(map[string]string)
	for _, val := range device.GetDevices() {
		err = val.LockNode(node, current)
		if err != nil {
			goto ReleaseNodeLocks
		}
	}

	tmppatch[util.DeviceBindPhase] = "allocating"
	tmppatch[util.BindTimeAnnotations] = strconv.FormatInt(time.Now().Unix(), 10)

	err = util.PatchPodAnnotations(current, tmppatch)
	if err != nil {
		klog.ErrorS(err, "patch pod annotation failed")
	}
	if err = s.kubeClient.CoreV1().Pods(args.PodNamespace).Bind(context.Background(), binding, metav1.CreateOptions{}); err != nil {
		klog.ErrorS(err, "Failed to bind pod", "pod", args.PodName, "namespace", args.PodNamespace, "podUID", args.PodUID, "node", args.Node)
	}
	if err == nil {
		s.recordScheduleBindingResultEvent(current, EventReasonBindingSucceed, []string{args.Node}, nil)
		res = &extenderv1.ExtenderBindingResult{
			Error: "",
		}
		klog.Infoln("After Binding Process")
		return res, nil
	}
ReleaseNodeLocks:
	klog.InfoS("bind failed", "err", err.Error())
	for _, val := range device.GetDevices() {
		val.ReleaseNodeLock(node, current)
	}
	s.recordScheduleBindingResultEvent(current, EventReasonBindingFailed, []string{}, err)
	return &extenderv1.ExtenderBindingResult{
		Error: err.Error(),
	}, nil
}

func (s *Scheduler) Filter(args extenderv1.ExtenderArgs) (*extenderv1.ExtenderFilterResult, error) {
	klog.InfoS("begin schedule filter", "pod", args.Pod.Name, "uuid", args.Pod.UID, "namespaces", args.Pod.Namespace)
	nums := k8sutil.Resourcereqs(args.Pod)
	total := 0
	for _, n := range nums {
		for _, k := range n {
			total += int(k.Nums)
		}
	}
	if total == 0 {
		klog.V(1).Infof("pod %v not find resource", args.Pod.Name)
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, []string{}, fmt.Errorf("does not request any resource"))
		return &extenderv1.ExtenderFilterResult{
			NodeNames:   args.NodeNames,
			FailedNodes: nil,
			Error:       "",
		}, nil
	}
	annos := args.Pod.Annotations
	s.delPod(args.Pod)
	nodeUsage, failedNodes, err := s.getNodesUsage(args.NodeNames, args.Pod)
	if err != nil {
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, []string{}, err)
		return nil, err
	}
	if len(failedNodes) != 0 {
		klog.V(5).InfoS("getNodesUsage failed nodes", "nodes", failedNodes)
	}
	nodeScores, err := s.calcScore(nodeUsage, nums, annos, args.Pod, failedNodes)
	if err != nil {
		err := fmt.Errorf("calcScore failed %v for pod %v", err, args.Pod.Name)
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, []string{}, err)
		return nil, err
	}
	if len((*nodeScores).NodeList) == 0 {
		klog.V(4).Infof("All node scores do not meet for pod %v", args.Pod.Name)
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, []string{}, fmt.Errorf("no available node, all node scores do not meet"))
		return &extenderv1.ExtenderFilterResult{
			FailedNodes: failedNodes,
		}, nil
	}
	klog.V(4).Infoln("nodeScores_len=", len((*nodeScores).NodeList))
	sort.Sort(nodeScores)
	m := (*nodeScores).NodeList[len((*nodeScores).NodeList)-1]
	klog.Infof("schedule %v/%v to %v %v", args.Pod.Namespace, args.Pod.Name, m.NodeID, m.Devices)
	annotations := make(map[string]string)
	annotations[util.AssignedNodeAnnotations] = m.NodeID
	annotations[util.AssignedTimeAnnotations] = strconv.FormatInt(time.Now().Unix(), 10)

	for _, val := range device.GetDevices() {
		val.PatchAnnotations(&annotations, m.Devices)
	}

	//InRequestDevices := util.EncodePodDevices(util.InRequestDevices, m.devices)
	//supportDevices := util.EncodePodDevices(util.SupportDevices, m.devices)
	//maps.Copy(annotations, InRequestDevices)
	//maps.Copy(annotations, supportDevices)
	s.addPod(args.Pod, m.NodeID, m.Devices)
	err = util.PatchPodAnnotations(args.Pod, annotations)
	if err != nil {
		s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringFailed, []string{}, err)
		s.delPod(args.Pod)
		return nil, err
	}
	s.recordScheduleFilterResultEvent(args.Pod, EventReasonFilteringSucceed, []string{m.NodeID}, nil)
	res := extenderv1.ExtenderFilterResult{NodeNames: &[]string{m.NodeID}}
	return &res, nil
}
