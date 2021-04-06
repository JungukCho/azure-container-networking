// Copyright 2018 Microsoft. All rights reserved.
// MIT License
package npm

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/npm/ipsm"
	"github.com/Azure/azure-container-networking/npm/metrics"
	"github.com/Azure/azure-container-networking/npm/util"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	networkinginformers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/kubernetes"
	netpollister "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type networkPolicyController struct {
	clientset          kubernetes.Interface
	netPolLister       netpollister.NetworkPolicyLister
	netPolListerSynced cache.InformerSynced
	workqueue          workqueue.RateLimitingInterface
	// (TODO): networkPolController does not need to have whole NetworkPolicyManager pointer. Need to improve it
	npMgr                  *NetworkPolicyManager
	isAzureNpmChainCreated bool
	// (TODO): why do we have this bool variable even though isAzureNpmChainCreated exists.
	isSafeToCleanUpAzureNpmChain bool
}

func NewNetworkPolicyController(npInformer networkinginformers.NetworkPolicyInformer, clientset kubernetes.Interface, npMgr *NetworkPolicyManager) *networkPolicyController {
	netPolController := &networkPolicyController{
		clientset:                    clientset,
		netPolLister:                 npInformer.Lister(),
		netPolListerSynced:           npInformer.Informer().HasSynced,
		workqueue:                    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "NetPols"),
		npMgr:                        npMgr,
		isAzureNpmChainCreated:       false,
		isSafeToCleanUpAzureNpmChain: false,
	}

	npInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    netPolController.addNetPol,
			UpdateFunc: netPolController.updateNetPol,
			DeleteFunc: netPolController.deleteNetPol,
		},
	)
	return netPolController
}

// filter this event if we do not need to handle this event
func (c *networkPolicyController) needSync(obj interface{}) (string, error) {
	var key string
	_, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok {
		return key, fmt.Errorf("cannot cast obj (%v) to network policy obj", obj)
	}

	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		return key, fmt.Errorf("error due to %s", err)
	}

	return key, nil
}

func (c *networkPolicyController) addNetPol(obj interface{}) {
	key, err := c.needSync(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	// (TODO): need to remove
	log.Logf("[NETPOL ADD EVENT] add key %s into workqueue", key)
	c.workqueue.Add(key)
}

func (c *networkPolicyController) updateNetPol(old, new interface{}) {
	key, err := c.needSync(new)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	// needSync already checked validation of casting new network policy.
	newNetPol, _ := new.(*networkingv1.NetworkPolicy)
	oldNetPol, ok := old.(*networkingv1.NetworkPolicy)
	if ok {
		if oldNetPol.ResourceVersion == newNetPol.ResourceVersion {
			// Periodic resync will send update events for all known network plicies.
			// Two different versions of the same network policy will always have different RVs.
			return
		}
	}

	c.workqueue.Add(key)
}

func (c *networkPolicyController) deleteNetPol(obj interface{}) {
	_, ok := obj.(*networkingv1.NetworkPolicy)
	// DeleteFunc gets the final state of the resource (if it is known).
	// Otherwise, it gets an object of type DeletedFinalStateUnknown.
	// This can happen if the watch is closed and misses the delete event and
	// the controller doesn't notice the deletion until the subsequent re-list
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[NETPOL DELETE EVENT] Received unexpected object type: %v", obj)
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}

		if _, ok = tombstone.Obj.(*networkingv1.NetworkPolicy); !ok {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[NETPOL DELETE EVENT] Received unexpected object type (error decoding object tombstone, invalid type): %v", obj)
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
	}

	log.Logf("[NETPOL DELETE EVENT]")
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	netPolCachedKey := util.GetNSNameWithPrefix(key)

	// (TODO): Reduce scope of lock later
	c.npMgr.Lock()
	defer c.npMgr.Unlock()
	_, netPolExists := c.npMgr.RawNpMap[netPolCachedKey]

	// If network policy object is not in the RawNpMap, we do not need to clean-up states for this network policy
	// since netPolController did not apply for any states for this pod
	if !netPolExists {
		return
	}

	log.Logf("[NETPOL DEL EVENT] add key %s into workqueue", key)
	c.workqueue.Add(key)
}

func (c *networkPolicyController) Run(threadiness int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	log.Logf("Starting Network Policy %d worker(s)", threadiness)
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	log.Logf("Started Network Policy workers")
	<-stopCh
	log.Logf("Shutting down Network Policy workers")

	return nil
}

func (c *networkPolicyController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *networkPolicyController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Pod resource to be synced.
		// TODO : may consider using "c.queue.AddAfter(key, *requeueAfter)" according to error type later
		if err := c.syncNetPol(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		log.Logf("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		metrics.SendErrorLogAndMetric(util.NetpolID, "syncNetPol error due to %v", err)
		return true
	}

	return true
}

// syncNetPol compares the actual state with the desired, and attempts to converge the two.
func (c *networkPolicyController) syncNetPol(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	log.Logf("SyncNetPol %s %s %s", key, namespace, name)
	// Get the network policy resource with this namespace/name
	netPolObj, err := c.netPolLister.NetworkPolicies(namespace).Get(name)

	// (TODO): Reduce scope of lock later
	c.npMgr.Lock()
	defer c.npMgr.Unlock()

	if err != nil {
		if errors.IsNotFound(err) {
			// (TODO): think the location of this code.
			log.Logf("Network Policy %s is not found, may be it is deleted", key)

			// netPolObj is not found, but should need to check the RawNpMap cache with cachedNetPolKey
			// #1 No item in RawNpMap, which means network policy with the cachedNetPolKey is not applied. Thus, do not need to clean up process.
			// #2 item in RawNpMap exists, which means network policy with the cachedNetPolKey is applied . Thus, Need to clean up process.
			cachedNetPolKey := util.GetNSNameWithPrefix(key)
			cachedNetPolObj, cachedNetPolObjExists := c.npMgr.RawNpMap[cachedNetPolKey]
			if !cachedNetPolObjExists {
				return nil
			}

			err = c.deleteNetworkPolicy(cachedNetPolObj)
			if err != nil {
				return fmt.Errorf("[syncNetPol] Error: %v when network policy is not found\n", err)
			}
		}
		return err
	}

	// If DeletionTimestamp of the netPolObj is set, start cleaning up lastly applied states.
	// This is early cleaning up process from updateNetPol event
	if netPolObj.ObjectMeta.DeletionTimestamp == nil && netPolObj.ObjectMeta.DeletionGracePeriodSeconds == nil {
		err = c.deleteNetworkPolicy(netPolObj)
		if err != nil {
			return fmt.Errorf("Error: %v when ObjectMeta.DeletionTimestamp field is set\n", err)
		}
	}

	// Install default rules for kube-system and iptables
	// (TODO): this default rules for kube-system and azure-npm chains, not directly related to passed netPolObj
	// How to decouple this call with passed netPolObj? since it causes unnecessary re-try for the passed netPolObj
	if err = c.initializeDefaultAzureNpmChain(); err != nil {
		return fmt.Errorf("[syncNetPol] Error: due to %v", err)
	}

	err = c.syncAddAndUpdateNetPol(netPolObj)
	if err != nil {
		return fmt.Errorf("[syncNetPol] Error due to  %s\n", err.Error())
	}

	return nil
}

// initializeDefaultAzureNpmChain install default rules for kube-system and iptables
func (c *networkPolicyController) initializeDefaultAzureNpmChain() error {
	if c.isAzureNpmChainCreated {
		return nil
	}

	ipsMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	iptMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].iptMgr
	if err := ipsMgr.CreateSet(util.KubeSystemFlag, append([]string{util.IpsetNetHashFlag})); err != nil {
		return fmt.Errorf("[initializeDefaultAzureNpmChain] Error: failed to initialize kube-system ipset with err %s", err)
	}
	if err := iptMgr.InitNpmChains(); err != nil {
		return fmt.Errorf("[initializeDefaultAzureNpmChain] Error: failed to initialize azure-npm chains with err %s", err)
	}

	c.isAzureNpmChainCreated = true
	return nil
}

// DeleteNetworkPolicy handles deleting network policy based on cachedNetPolKey.
func (c *networkPolicyController) deleteNetworkPolicy(netPolObj *networkingv1.NetworkPolicy) error {
	var err error
	netpolKey, err := cache.MetaNamespaceKeyFunc(netPolObj)
	if err != nil {
		return fmt.Errorf("[deleteNetworkPolicy] Error: while running MetaNamespaceKeyFunc err: %s", err)
	}

	cachedNetPolKey := util.GetNSNameWithPrefix(netpolKey)
	cachedNetPolObj, cachedNetPolObjExists := c.npMgr.RawNpMap[cachedNetPolKey]
	// if there is no applied network policy with the cachedNetPolKey, do not need to clean up process.
	if !cachedNetPolObjExists {
		return nil
	}

	ipsMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	iptMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].iptMgr
	// translate policy from "cachedNetPolObj"
	_, _, _, ingressIPCidrs, egressIPCidrs, iptEntries := translatePolicy(cachedNetPolObj)

	// delete iptables entries
	for _, iptEntry := range iptEntries {
		if err = iptMgr.Delete(iptEntry); err != nil {
			return fmt.Errorf("[deleteNetworkPolicy] Error: failed to apply iptables rule. Rule: %+v with err: %v", iptEntry, err)
		}
	}

	// delete ipset list related to ingress CIDRs
	if err = c.removeCidrsRule("in", netPolObj.ObjectMeta.Name, netPolObj.ObjectMeta.Namespace, ingressIPCidrs, ipsMgr); err != nil {
		return fmt.Errorf("[deleteNetworkPolicy] Error: removeCidrsRule in due to %v", err)
	}

	// delete ipset list related to egress CIDRs
	if err = c.removeCidrsRule("out", netPolObj.ObjectMeta.Name, netPolObj.ObjectMeta.Namespace, egressIPCidrs, ipsMgr); err != nil {
		return fmt.Errorf("[deleteNetworkPolicy] Error: removeCidrsRule out due to %v", err)
	}

	delete(c.npMgr.RawNpMap, cachedNetPolKey)

	// (TODO): it is not related to event - need to separate it.
	if c.canCleanUpNpmChains() {
		c.isAzureNpmChainCreated = false
		if err = iptMgr.UninitNpmChains(); err != nil {
			return fmt.Errorf("[deleteNetworkPolicy] Error: failed to uninitialize azure-npm chains with err: %s", err)
		}
	}

	metrics.NumPolicies.Dec()
	return nil
}

// Add and Update NetworkPolicy handles adding network policy to iptables.
func (c *networkPolicyController) syncAddAndUpdateNetPol(netPolObj *networkingv1.NetworkPolicy) error {
	timer := metrics.StartNewTimer()

	var err error
	netpolKey, err := cache.MetaNamespaceKeyFunc(netPolObj)
	if err != nil {
		return fmt.Errorf("[syncAddAndUpdateNetPol] Error: while running MetaNamespaceKeyFunc err: %s", err)
	}

	// Start reconciling loop to eventually meet the desired states from network policy
	// #1 If a new network policy is created, the network policy is not in RawNPMap.
	// start translating policy and install translated ipset and iptables rules into kernel
	// #2 If a network policy with <ns>-<netpol namespace>-<netpol name> is applied before and two network policy are the same object (same UID),
	// first delete the applied network policy,
	// then start translating policy and install translated ipset and iptables rules into kernel
	// #3 If a network policy with <ns>-<netpol namespace>-<netpol name> is applied before and two network policy are the different object (different UID) due to missing some events for the old object
	// first delete the applied network policy,
	// then start translating policy and install translated ipset and iptables rules into kernel
	// For #2 and #3, the logic are the same.

	// (TODO): Need more optimizations
	// Need to apply difference only if possible

	err = c.deleteNetworkPolicy(netPolObj)
	if err != nil {
		return fmt.Errorf("[syncAddAndUpdateNetPol] Error: failed to deleteNetworkPolicy due to %s", err)
	}

	ipsMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	iptMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].iptMgr
	sets, namedPorts, lists, ingressIPCidrs, egressIPCidrs, iptEntries := translatePolicy(netPolObj)
	for _, set := range sets {
		log.Logf("Creating set: %v, hashedSet: %v", set, util.GetHashedName(set))
		if err = ipsMgr.CreateSet(set, append([]string{util.IpsetNetHashFlag})); err != nil {
			return fmt.Errorf("[syncAddAndUpdateNetPol] Error: creating ipset %s with err: %v", set, err)
		}
	}
	for _, set := range namedPorts {
		log.Logf("Creating set: %v, hashedSet: %v", set, util.GetHashedName(set))
		if err = ipsMgr.CreateSet(set, append([]string{util.IpsetIPPortHashFlag})); err != nil {
			return fmt.Errorf("[syncAddAndUpdateNetPol] Error: creating ipset named port %s with err: %v", set, err)
		}
	}
	for _, list := range lists {
		if err = ipsMgr.CreateList(list); err != nil {
			return fmt.Errorf("[syncAddAndUpdateNetPol] Error: creating ipset list %s with err: %v", list, err)
		}
	}

	// (TODO): Do we need initAllNsList??
	// Can we move this into "initializeDefaultAzureNpmChain" function?
	if err = c.initAllNsList(); err != nil {
		return fmt.Errorf("[syncAddAndUpdateNetPol] Error: initializing all-namespace ipset list with err: %v", err)
	}

	if err = c.createCidrsRule("in", netPolObj.ObjectMeta.Name, netPolObj.ObjectMeta.Namespace, ingressIPCidrs, ipsMgr); err != nil {
		return fmt.Errorf("[syncAddAndUpdateNetPol] Error: createCidrsRule in due to %v", err)
	}

	if err = c.createCidrsRule("out", netPolObj.ObjectMeta.Name, netPolObj.ObjectMeta.Namespace, egressIPCidrs, ipsMgr); err != nil {
		return fmt.Errorf("[syncAddAndUpdateNetPol] Error: createCidrsRule out due to %v", err)
	}

	for _, iptEntry := range iptEntries {
		if err = iptMgr.Add(iptEntry); err != nil {
			return fmt.Errorf("[syncAddAndUpdateNetPol] Error: failed to apply iptables rule. Rule: %+v with err: %v", iptEntry, err)
		}
	}

	cachedNetPolKey := util.GetNSNameWithPrefix(netpolKey)
	c.npMgr.RawNpMap[cachedNetPolKey] = netPolObj

	metrics.NumPolicies.Inc()

	// (TODO): may better to use defer?
	timer.StopAndRecord(metrics.AddPolicyExecTime)
	return nil
}

func (c *networkPolicyController) createCidrsRule(ingressOrEgress, policyName, ns string, ipsetEntries [][]string, ipsMgr *ipsm.IpsetManager) error {
	spec := append([]string{util.IpsetNetHashFlag, util.IpsetMaxelemName, util.IpsetMaxelemNum})

	for i, ipCidrSet := range ipsetEntries {
		if ipCidrSet == nil || len(ipCidrSet) == 0 {
			continue
		}
		setName := policyName + "-in-ns-" + ns + "-" + strconv.Itoa(i) + ingressOrEgress
		log.Logf("Creating set: %v, hashedSet: %v", setName, util.GetHashedName(setName))
		if err := ipsMgr.CreateSet(setName, spec); err != nil {
			metrics.SendErrorLogAndMetric(util.NetpolID, "[createCidrsRule] Error: creating ipset %s with err: %v", ipCidrSet, err)
		}
		for _, ipCidrEntry := range util.DropEmptyFields(ipCidrSet) {
			// Ipset doesn't allow 0.0.0.0/0 to be added. A general solution is split 0.0.0.0/1 in half which convert to
			// 1.0.0.0/1 and 128.0.0.0/1
			if ipCidrEntry == "0.0.0.0/0" {
				splitEntry := [2]string{"1.0.0.0/1", "128.0.0.0/1"}
				for _, entry := range splitEntry {
					if err := ipsMgr.AddToSet(setName, entry, util.IpsetNetHashFlag, ""); err != nil {
						return fmt.Errorf("[createCidrsRule] adding ip cidrs %s into ipset %s with err: %v", entry, ipCidrSet, err)
					}
				}
			} else {
				if err := ipsMgr.AddToSet(setName, ipCidrEntry, util.IpsetNetHashFlag, ""); err != nil {
					return fmt.Errorf("[createCidrsRule] adding ip cidrs %s into ipset %s with err: %v", ipCidrEntry, ipCidrSet, err)
				}
			}
		}
	}

	return nil
}

func (c *networkPolicyController) removeCidrsRule(ingressOrEgress, policyName, ns string, ipsetEntries [][]string, ipsMgr *ipsm.IpsetManager) error {
	for i, ipCidrSet := range ipsetEntries {
		if ipCidrSet == nil || len(ipCidrSet) == 0 {
			continue
		}
		setName := policyName + "-in-ns-" + ns + "-" + strconv.Itoa(i) + ingressOrEgress
		log.Logf("Delete set: %v, hashedSet: %v", setName, util.GetHashedName(setName))
		if err := ipsMgr.DeleteSet(setName); err != nil {
			return fmt.Errorf("[removeCidrsRule] deleting ipset %s with err: %v", ipCidrSet, err)
		}
	}

	return nil
}

func (c *networkPolicyController) canCleanUpNpmChains() bool {
	// (TODO): why do we have this?
	// if !c.isSafeToCleanUpAzureNpmChain {
	// 	return false
	// }

	if len(c.npMgr.RawNpMap) == 0 {
		return true
	}

	return false
}

// (TODO): copied from namespace.go - InitAllNsList syncs all-namespace ipset list
func (c *networkPolicyController) initAllNsList() error {
	// who adds util.KubeAllNamespacesFlag in NsMap?
	ipsMgr := c.npMgr.NsMap[util.KubeAllNamespacesFlag].IpsMgr
	for ns := range c.npMgr.NsMap {
		if ns == util.KubeAllNamespacesFlag {
			continue
		}

		if err := ipsMgr.AddToList(util.KubeAllNamespacesFlag, ns); err != nil {
			return fmt.Errorf("[InitAllNsList] Error: failed to add namespace set %s to ipset list %s with err: %v", ns, util.KubeAllNamespacesFlag, err)
		}
	}
	return nil
}

// (TODO): copied from namespace.go, but no component uses this function UninitAllNsList cleans all-namespace ipset list.
func (c *networkPolicyController) unInitAllNsList() error {
	allNs := c.npMgr.NsMap[util.KubeAllNamespacesFlag]
	for ns := range c.npMgr.NsMap {
		if ns == util.KubeAllNamespacesFlag {
			continue
		}

		if err := allNs.IpsMgr.DeleteFromList(util.KubeAllNamespacesFlag, ns); err != nil {
			return fmt.Errorf("[UninitAllNsList] Error: failed to delete namespace set %s from list %s with err: %v", ns, util.KubeAllNamespacesFlag, err)
		}
	}
	return nil
}

// GetProcessedNPKey will return netpolKey
func (c *networkPolicyController) getProcessedNPKey(netPolObj *networkingv1.NetworkPolicy) string {
	// hashSelector will never be empty
	// (TODO): what if PodSelector is [] or nothing? - make the Unit test for this
	hashedPodSelector := HashSelector(&netPolObj.Spec.PodSelector)

	// (TODO): any chance to have namespace has zero length?
	if len(netPolObj.GetNamespace()) > 0 {
		hashedPodSelector = netPolObj.GetNamespace() + "/" + hashedPodSelector
	}
	return util.GetNSNameWithPrefix(hashedPodSelector)
}

// (TODO): placeholder to avoid compile errors
func (npMgr *NetworkPolicyManager) AddNetworkPolicy(netPol *networkingv1.NetworkPolicy) error {
	return nil
}

func (npMgr *NetworkPolicyManager) DeleteNetworkPolicy(netPol *networkingv1.NetworkPolicy) error {
	return nil
}

func (npMgr *NetworkPolicyManager) UpdateNetworkPolicy(oldNpObj *networkingv1.NetworkPolicy, newNpObj *networkingv1.NetworkPolicy) error {
	return nil
}
