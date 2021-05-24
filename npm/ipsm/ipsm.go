// Package ipsm focus on ip set operation
// Copyright 2018 Microsoft. All rights reserved.
// MIT License
package ipsm

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/npm/metrics"
	"github.com/Azure/azure-container-networking/npm/util"
)

/*
TODO:
1. Clarify error codes after running ipset commands and need to use it in a very accurate way
2. Try to decouple listMap and setMap if possible. At least use different locks for listMap and SetMa
3. Clean up codes (i.e., just use different functions for "Nethash" and "List" type of IPSet to maintain code better).
This will also helps to reduce the scope of locks
*/
type ipsEntry struct {
	operationFlag string
	name          string
	set           string
	spec          []string
}

// IpsetManager stores ipset states.
type IpsetManager struct {
	listMap map[string]*ipset //tracks all set lists.
	setMap  map[string]*ipset //label -> []ip
	sync.Mutex
}

// Ipset represents one ipset entry.
type ipset struct {
	name       string
	elements   map[string]string // key = ip, value: context associated to the ip like podKey
	referCount int
}

// NewIpset creates a new instance for Ipset object.
func newIpset(setName string) *ipset {
	return &ipset{
		name:     setName,
		elements: make(map[string]string),
	}
}

// NewIpsetManager creates a new instance for IpsetManager object.
func NewIpsetManager() *IpsetManager {
	return &IpsetManager{
		listMap: make(map[string]*ipset),
		setMap:  make(map[string]*ipset),
	}
}

// Exists checks if an element exists in setMap/listMap.
func (ipsMgr *IpsetManager) exists(listName string, setName string, kind string) bool {
	m := ipsMgr.setMap
	if kind == util.IpsetSetListFlag {
		m = ipsMgr.listMap
	}

	if _, exists := m[listName]; !exists {
		return false
	}

	if _, exists := m[listName].elements[setName]; !exists {
		return false
	}

	return true
}

// SetExists checks if an ipset exists, and returns the type
func (ipsMgr *IpsetManager) setExists(setName string) (bool, string) {
	_, exists := ipsMgr.setMap[setName]
	if exists {
		return exists, util.IpsetSetGenericFlag
	}

	_, exists = ipsMgr.listMap[setName]
	if exists {
		return exists, util.IpsetSetListFlag
	}

	return exists, ""
}

// (TODO): delete. not used in any other places
func isNsSet(setName string) bool {
	return !strings.Contains(setName, "-") && !strings.Contains(setName, ":")
}

// DeleteList removes an ipset list.
func (ipsMgr *IpsetManager) deleteList(listName string) error {
	entry := &ipsEntry{
		operationFlag: util.IpsetDestroyFlag,
		set:           util.GetHashedName(listName),
	}

	if errCode, err := ipsMgr.run(entry); err != nil {
		if errCode == 1 {
			return nil
		}

		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to delete ipset %s %+v", listName, entry)
		return err
	}

	delete(ipsMgr.listMap, listName)
	return nil
}

// Run execute an ipset command to update ipset.
func (ipsMgr *IpsetManager) run(entry *ipsEntry) (int, error) {
	cmdName := util.Ipset
	cmdArgs := append([]string{entry.operationFlag, util.IpsetExistFlag, entry.set}, entry.spec...)
	cmdArgs = util.DropEmptyFields(cmdArgs)

	log.Logf("Executing ipset command %s %v", cmdName, cmdArgs)
	_, err := exec.Command(cmdName, cmdArgs...).Output()
	if msg, failed := err.(*exec.ExitError); failed {
		errCode := msg.Sys().(syscall.WaitStatus).ExitStatus()
		if errCode > 0 {
			metrics.SendErrorLogAndMetric(util.IpsmID, "Error: There was an error running command: [%s %v] Stderr: [%v, %s]", cmdName, strings.Join(cmdArgs, " "), err, strings.TrimSuffix(string(msg.Stderr), "\n"))
		}

		return errCode, err
	}

	return 0, nil
}

// to avoid trying to hold lock multiple times (e.g., AddToList called CreateList)
func (ipsMgr *IpsetManager) createList(listName string) error {
	if _, exists := ipsMgr.listMap[listName]; exists {
		return nil
	}

	entry := &ipsEntry{
		name:          listName,
		operationFlag: util.IpsetCreationFlag,
		set:           util.GetHashedName(listName),
		spec:          []string{util.IpsetSetListFlag},
	}
	log.Logf("Creating List: %+v", entry)
	if errCode, err := ipsMgr.run(entry); err != nil && errCode != 1 {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to create ipset list %s.", listName)
		return err
	}

	ipsMgr.listMap[listName] = newIpset(listName)
	return nil
}

// Destroy completely cleans ipset.
func (ipsMgr *IpsetManager) destroy() error {
	entry := &ipsEntry{
		operationFlag: util.IpsetFlushFlag,
	}
	if _, err := ipsMgr.run(entry); err != nil {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to flush ipset")
		return err
	}

	entry.operationFlag = util.IpsetDestroyFlag
	if _, err := ipsMgr.run(entry); err != nil {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to destroy ipset")
		return err
	}

	//TODO set IPSetInventory to 0 for all set names
	return nil
}

// CreateSet creates an ipset.
func (ipsMgr *IpsetManager) createSet(setName string, spec []string) error {
	timer := metrics.StartNewTimer()

	if _, exists := ipsMgr.setMap[setName]; exists {
		return nil
	}

	entry := &ipsEntry{
		name:          setName,
		operationFlag: util.IpsetCreationFlag,
		// Use hashed string for set name to avoid string length limit of ipset.
		set:  util.GetHashedName(setName),
		spec: spec,
	}
	log.Logf("Creating Set: %+v", entry)
	// (TODO): need to differentiate errCode handler
	// since errCode can be one in case of "set with the same name already exists" and "maximal number of sets reached, cannot create more."
	// It may have more situations with errCode==1.
	if errCode, err := ipsMgr.run(entry); err != nil && errCode != 1 {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to create ipset.")
		return err
	}

	ipsMgr.setMap[setName] = newIpset(setName)

	metrics.NumIPSets.Inc()
	timer.StopAndRecord(metrics.AddIPSetExecTime)
	metrics.SetIPSetInventory(setName, 0)

	return nil
}

func (ipsMgr *IpsetManager) deleteSet(setName string) error {
	if _, exists := ipsMgr.setMap[setName]; !exists {
		metrics.SendErrorLogAndMetric(util.IpsmID, "ipset with name %s not found", setName)
		return nil
	}

	entry := &ipsEntry{
		operationFlag: util.IpsetDestroyFlag,
		set:           util.GetHashedName(setName),
	}

	if errCode, err := ipsMgr.run(entry); err != nil {
		if errCode == 1 {
			return nil
		}

		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to delete ipset %s. Entry: %+v", setName, entry)
		return err
	}

	delete(ipsMgr.setMap, setName)

	metrics.NumIPSets.Dec()
	metrics.NumIPSetEntries.Add(float64(-metrics.GetIPSetInventory(setName)))
	metrics.SetIPSetInventory(setName, 0)

	return nil
}

// CreateList creates an ipset list. npm maintains one setlist per namespace label.
func (ipsMgr *IpsetManager) CreateList(listName string) error {
	// (TODO): Need more fine-grained lock management after understanding how it used from each controller
	ipsMgr.Lock()
	defer ipsMgr.Unlock()
	return ipsMgr.createList(listName)
}

// AddToList inserts an ipset to an ipset list.
func (ipsMgr *IpsetManager) AddToList(listName string, setName string) error {
	// (TODO): Need more fine-grained lock management after understanding how it used from each controller
	ipsMgr.Lock()
	defer ipsMgr.Unlock()

	if listName == setName {
		return nil
	}

	//Check if list being added exists in the listMap, if it exists we don't care about the set type
	exists, _ := ipsMgr.setExists(setName)

	// if set does not exist, then return because the ipset call will fail due to set not existing
	if !exists {
		return fmt.Errorf("Set [%s] does not exist when attempting to add to list [%s]", setName, listName)
	}

	// Check if the list that is being added to exists
	exists, listtype := ipsMgr.setExists(listName)

	// Make sure that set returned is of list type, otherwise return because we can't add a set to a non setlist type
	if exists && listtype != util.IpsetSetListFlag {
		return fmt.Errorf("Failed to add set [%s] to list [%s], but list is of type [%s]", setName, listName, listtype)
	} else if !exists {
		// if the list doesn't exist, create it
		if err := ipsMgr.createList(listName); err != nil {
			return err
		}
	}

	// check if set already exists in the list
	if ipsMgr.exists(listName, setName, util.IpsetSetListFlag) {
		return nil
	}

	entry := &ipsEntry{
		operationFlag: util.IpsetAppendFlag,
		set:           util.GetHashedName(listName),
		spec:          []string{util.GetHashedName(setName)},
	}

	// add set to list
	if errCode, err := ipsMgr.run(entry); err != nil && errCode != 1 {
		return fmt.Errorf("Error: failed to create ipset rules. rule: %+v, error: %v", entry, err)
	}

	ipsMgr.listMap[listName].elements[setName] = ""

	return nil
}

// DeleteFromList removes an ipset to an ipset list.
func (ipsMgr *IpsetManager) DeleteFromList(listName string, setName string) error {
	// (TODO): Need more fine-grained lock management after understanding how it used from each controller
	ipsMgr.Lock()
	defer ipsMgr.Unlock()

	//Check if list being added exists in the listMap, if it exists we don't care about the set type
	exists, _ := ipsMgr.setExists(setName)

	// if set does not exist, then return because the ipset call will fail due to set not existing
	// TODO make sure these are info and not errors, use NPmErr
	if !exists {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Set [%s] does not exist when attempting to delete from list [%s]", setName, listName)
		return nil
	}

	//Check if list being added exists in the listMap, if it exists we don't care about the set type
	exists, listtype := ipsMgr.setExists(listName)

	// if set does not exist, then return because the ipset call will fail due to set not existing
	if !exists {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Set [%s] does not exist when attempting to add to list [%s]", setName, listName)
		return nil
	}

	if listtype != util.IpsetSetListFlag {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Set [%s] is of the wrong type when attempting to delete list [%s], actual type [%s]", setName, listName, listtype)
		return nil
	}

	if _, exists := ipsMgr.listMap[listName]; !exists {
		metrics.SendErrorLogAndMetric(util.IpsmID, "ipset list with name %s not found", listName)
		return nil
	}

	hashedListName, hashedSetName := util.GetHashedName(listName), util.GetHashedName(setName)
	entry := &ipsEntry{
		operationFlag: util.IpsetDeletionFlag,
		set:           hashedListName,
		spec:          []string{hashedSetName},
	}

	if _, err := ipsMgr.run(entry); err != nil {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to delete ipset entry. %+v", entry)
		return err
	}

	// Now cleanup the cache
	if _, exists := ipsMgr.listMap[listName].elements[setName]; exists {
		delete(ipsMgr.listMap[listName].elements, setName)
	}

	if len(ipsMgr.listMap[listName].elements) == 0 {
		if err := ipsMgr.deleteList(listName); err != nil {
			metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to delete ipset list %s.", listName)
			return err
		}
	}

	return nil
}

// CreateSet creates an ipset.
func (ipsMgr *IpsetManager) CreateSet(setName string, spec []string) error {
	// (TODO): Need more fine-grained lock management after understanding how it used from each controller
	ipsMgr.Lock()
	defer ipsMgr.Unlock()
	return ipsMgr.createSet(setName, spec)
}

// DeleteSet removes a set from ipset.
func (ipsMgr *IpsetManager) DeleteSet(setName string) error {
	// (TODO): Need more fine-grained lock management after understanding how it used from each controller
	ipsMgr.Lock()
	defer ipsMgr.Unlock()
	return ipsMgr.deleteSet(setName)
}

// AddToSet inserts an ip to an entry in setMap, and creates/updates the corresponding ipset.
func (ipsMgr *IpsetManager) AddToSet(setName, ip, spec, podKey string) error {
	// (TODO): Need more fine-grained lock management after understanding how it used from each controller
	ipsMgr.Lock()
	defer ipsMgr.Unlock()

	if ipsMgr.exists(setName, ip, spec) {
		// make sure we have updated the podKey in case it gets changed
		cachedPodKey := ipsMgr.setMap[setName].elements[ip]
		if cachedPodKey != podKey {
			log.Logf("AddToSet: PodOwner has changed for Ip: %s, setName:%s, Old podKey: %s, new podKey: %s. Replace context with new PodOwner.",
				ip, setName, cachedPodKey, podKey)

			ipsMgr.setMap[setName].elements[ip] = podKey
		}

		return nil
	}

	// possible formats
	//192.168.0.1
	//192.168.0.1,tcp:25227
	// todo: handle ip and port with protocol, plus just ip
	// always guaranteed to have ip, not guaranteed to have port + protocol
	ipDetails := strings.Split(ip, ",")
	if len(ipDetails) > 0 && ipDetails[0] == "" {
		return fmt.Errorf("Failed to add IP to set [%s], the ip to be added was empty, spec: %+v", setName, spec)
	}

	// check if the set exists, ignore the type of the set being added if it exists since the only purpose is to see if it's created or not
	exists, _ := ipsMgr.setExists(setName)

	if !exists {
		if err := ipsMgr.createSet(setName, append([]string{spec})); err != nil {
			return err
		}
	}

	var resultSpec []string
	if strings.Contains(ip, util.IpsetNomatch) {
		ip = strings.Trim(ip, util.IpsetNomatch)
		resultSpec = append([]string{ip, util.IpsetNomatch})
	} else {
		resultSpec = append([]string{ip})
	}

	entry := &ipsEntry{
		operationFlag: util.IpsetAppendFlag,
		set:           util.GetHashedName(setName),
		spec:          resultSpec,
	}

	// todo: check err handling besides error code, corrupt state possible here
	if errCode, err := ipsMgr.run(entry); err != nil && errCode != 1 {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to create ipset rules. %+v", entry)
		return err
	}

	// Stores the podKey as the context for this ip.
	ipsMgr.setMap[setName].elements[ip] = podKey

	metrics.NumIPSetEntries.Inc()
	metrics.IncIPSetInventory(setName)

	return nil
}

// DeleteFromSet removes an ip from an entry in setMap, and delete/update the corresponding ipset.
func (ipsMgr *IpsetManager) DeleteFromSet(setName, ip, podKey string) error {
	// (TODO): Need more fine-grained lock management after understanding how it used from each controller
	ipsMgr.Lock()
	defer ipsMgr.Unlock()

	ipSet, exists := ipsMgr.setMap[setName]
	if !exists {
		log.Logf("ipset with name %s not found", setName)
		return nil
	}

	// possible formats
	//192.168.0.1
	//192.168.0.1,tcp:25227
	// todo: handle ip and port with protocol, plus just ip
	// always guaranteed to have ip, not guaranteed to have port + protocol
	ipDetails := strings.Split(ip, ",")
	if len(ipDetails) > 0 && ipDetails[0] == "" {
		return fmt.Errorf("Failed to add IP to set [%s], the ip to be added was empty", setName)
	}

	if _, exists := ipsMgr.setMap[setName].elements[ip]; exists {
		// in case the IP belongs to a new Pod, then ignore this Delete call as this might be stale
		cachedPodKey := ipSet.elements[ip]
		if cachedPodKey != podKey {
			log.Logf("DeleteFromSet: PodOwner has changed for Ip: %s, setName:%s, Old podKey: %s, new podKey: %s. Ignore the delete as this is stale update",
				ip, setName, cachedPodKey, podKey)

			return nil
		}
	}

	// TODO optimize to not run this command in case cache has already been updated.
	entry := &ipsEntry{
		operationFlag: util.IpsetDeletionFlag,
		set:           util.GetHashedName(setName),
		spec:          append([]string{ip}),
	}

	if errCode, err := ipsMgr.run(entry); err != nil {
		if errCode == 1 {
			return nil
		}

		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to delete ipset entry. Entry: %+v", entry)
		return err
	}

	// Now cleanup the cache
	delete(ipsMgr.setMap[setName].elements, ip)

	metrics.NumIPSetEntries.Dec()
	metrics.DecIPSetInventory(setName)

	if len(ipsMgr.setMap[setName].elements) == 0 {
		ipsMgr.deleteSet(setName)
	}

	return nil
}

// DestroyNpmIpsets destroys only ipsets created by NPM
func (ipsMgr *IpsetManager) DestroyNpmIpsets() error {
	// (TODO): Need more fine-grained lock management after understanding how it used from each controller
	ipsMgr.Lock()
	defer ipsMgr.Unlock()

	cmdName := util.Ipset
	cmdArgs := util.IPsetCheckListFlag

	reply, err := exec.Command(cmdName, cmdArgs).Output()
	if msg, failed := err.(*exec.ExitError); failed {
		errCode := msg.Sys().(syscall.WaitStatus).ExitStatus()
		if errCode > 0 {
			metrics.SendErrorLogAndMetric(util.IpsmID, "{DestroyNpmIpsets} Error: There was an error running command: [%s] Stderr: [%v, %s]", cmdName, err, strings.TrimSuffix(string(msg.Stderr), "\n"))
		}

		return err
	}
	if reply == nil {
		metrics.SendErrorLogAndMetric(util.IpsmID, "{DestroyNpmIpsets} Received empty string from ipset list while destroying azure-npm ipsets")
		return nil
	}

	re := regexp.MustCompile("Name: (" + util.AzureNpmPrefix + "\\d+)")
	ipsetRegexSlice := re.FindAllSubmatch(reply, -1)

	if len(ipsetRegexSlice) == 0 {
		log.Logf("No Azure-NPM IPsets are found in the Node.")
		return nil
	}

	ipsetLists := make([]string, 0)
	for _, matchedItem := range ipsetRegexSlice {
		if len(matchedItem) == 2 {
			itemString := string(matchedItem[1])
			if strings.Contains(itemString, util.AzureNpmFlag) {
				ipsetLists = append(ipsetLists, itemString)
			}
		}
	}

	if len(ipsetLists) == 0 {
		return nil
	}

	entry := &ipsEntry{
		operationFlag: util.IpsetFlushFlag,
	}

	for _, ipsetName := range ipsetLists {
		entry := &ipsEntry{
			operationFlag: util.IpsetFlushFlag,
			set:           ipsetName,
		}

		if _, err := ipsMgr.run(entry); err != nil {
			metrics.SendErrorLogAndMetric(util.IpsmID, "{DestroyNpmIpsets} Error: failed to flush ipset %s", ipsetName)
		}
	}

	for _, ipsetName := range ipsetLists {
		entry.operationFlag = util.IpsetDestroyFlag
		entry.set = ipsetName
		if _, err := ipsMgr.run(entry); err != nil {
			metrics.SendErrorLogAndMetric(util.IpsmID, "{DestroyNpmIpsets} Error: failed to destroy ipset %s", ipsetName)
		}
	}

	return nil
}

// only used in UT
// (TODO): may not need this function since npm will used exec-based UT
// Save saves ipset to file.
func (ipsMgr *IpsetManager) Save(configFile string) error {
	if len(configFile) == 0 {
		configFile = util.IpsetConfigFile
	}

	cmd := exec.Command(util.Ipset, util.IpsetSaveFlag, util.IpsetFileFlag, configFile)
	if err := cmd.Start(); err != nil {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to save ipset to file.")
		return err
	}
	cmd.Wait()

	return nil
}

// only used in UT
// (TODO): may not need this function since npm will used exec-based UT
// Restore restores ipset from file.
func (ipsMgr *IpsetManager) Restore(configFile string) error {
	if len(configFile) == 0 {
		configFile = util.IpsetConfigFile
	}

	f, err := os.Stat(configFile)
	if err != nil {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to get file %s stat from ipsm.Restore", configFile)
		return err
	}

	if f.Size() == 0 {
		if err := ipsMgr.destroy(); err != nil {
			return err
		}
	}

	cmd := exec.Command(util.Ipset, util.IpsetRestoreFlag, util.IpsetFileFlag, configFile)
	if err := cmd.Start(); err != nil {
		metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to to restore ipset from file.")
		return err
	}
	cmd.Wait()

	//TODO based on the set name and number of entries in the config file, update IPSetInventory
	return nil
}

// not used in any other codes
// Clean removes all the empty sets & lists under the namespace.
func (ipsMgr *IpsetManager) Clean() error {
	// (TODO): Need more fine-grained lock management after understanding how it used from each controller
	ipsMgr.Lock()
	defer ipsMgr.Unlock()
	for setName, set := range ipsMgr.setMap {
		if len(set.elements) > 0 {
			continue
		}

		if err := ipsMgr.deleteSet(setName); err != nil {
			metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to clean ipset")
			return err
		}
	}

	for listName, list := range ipsMgr.listMap {
		if len(list.elements) > 0 {
			continue
		}

		if err := ipsMgr.deleteList(listName); err != nil {
			metrics.SendErrorLogAndMetric(util.IpsmID, "Error: failed to clean ipset list")
			return err
		}
	}

	return nil
}
