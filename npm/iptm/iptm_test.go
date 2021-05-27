package iptm

import (
	"os"
	"testing"

	"github.com/Azure/azure-container-networking/npm/metrics"
	"github.com/Azure/azure-container-networking/npm/metrics/promutil"
	"github.com/Azure/azure-container-networking/npm/util"
)

func TestSave(t *testing.T) {
	iptMgr := &IptablesManager{}
	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestSave failed @ iptMgr.Save")
	}
}

func TestRestore(t *testing.T) {
	iptMgr := &IptablesManager{}
	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestRestore failed @ iptMgr.Save")
	}

	if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestRestore failed @ iptMgr.Restore")
	}
}

func TestInitNpmChains(t *testing.T) {
	iptMgr := &IptablesManager{}

	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestInitNpmChains failed @ iptMgr.Save")
	}

	defer func() {
		if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestInitNpmChains failed @ iptMgr.Restore")
		}
	}()

	if err := iptMgr.InitNpmChains(); err != nil {
		t.Errorf("TestInitNpmChains @ iptMgr.InitNpmChains")
	}
}

func TestUninitNpmChains(t *testing.T) {
	iptMgr := &IptablesManager{}

	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestUninitNpmChains failed @ iptMgr.Save")
	}

	defer func() {
		if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestUninitNpmChains failed @ iptMgr.Restore")
		}
	}()

	if err := iptMgr.InitNpmChains(); err != nil {
		t.Errorf("TestUninitNpmChains @ iptMgr.InitNpmChains")
	}

	if err := iptMgr.UninitNpmChains(); err != nil {
		t.Errorf("TestUninitNpmChains @ iptMgr.UninitNpmChains")
	}
}

func TestExists(t *testing.T) {
	iptMgr := &IptablesManager{}
	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestExists failed @ iptMgr.Save")
	}

	defer func() {
		if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestExists failed @ iptMgr.Restore")
		}
	}()

	iptMgr.OperationFlag = util.IptablesCheckFlag
	entry := &IptEntry{
		Chain: util.IptablesForwardChain,
		Specs: []string{
			util.IptablesJumpFlag,
			util.IptablesAccept,
		},
	}
	if _, err := iptMgr.exists(entry); err != nil {
		t.Errorf("TestExists failed @ iptMgr.exists")
	}
}

func TestAddChain(t *testing.T) {
	iptMgr := &IptablesManager{}
	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestAddChain failed @ iptMgr.Save")
	}

	defer func() {
		if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestAddChain failed @ iptMgr.Restore")
		}
	}()

	if err := iptMgr.addChain("TEST-CHAIN"); err != nil {
		t.Errorf("TestAddChain failed @ iptMgr.addChain")
	}
}

func TestDeleteChain(t *testing.T) {
	iptMgr := &IptablesManager{}
	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestDeleteChain failed @ iptMgr.Save")
	}

	defer func() {
		if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestDeleteChain failed @ iptMgr.Restore")
		}
	}()

	if err := iptMgr.addChain("TEST-CHAIN"); err != nil {
		t.Errorf("TestDeleteChain failed @ iptMgr.addChain")
	}

	if err := iptMgr.deleteChain("TEST-CHAIN"); err != nil {
		t.Errorf("TestDeleteChain failed @ iptMgr.deleteChain")
	}
}

func TestAdd(t *testing.T) {
	iptMgr := &IptablesManager{}
	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestAdd failed @ iptMgr.Save")
	}

	defer func() {
		if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestAdd failed @ iptMgr.Restore")
		}
	}()

	entry := &IptEntry{
		Chain: util.IptablesForwardChain,
		Specs: []string{
			util.IptablesJumpFlag,
			util.IptablesReject,
		},
	}

	gaugeVal, err1 := promutil.GetValue(metrics.NumIPTableRules)
	countVal, err2 := promutil.GetCountValue(metrics.AddIPTableRuleExecTime)

	if err := iptMgr.Add(entry); err != nil {
		t.Errorf("TestAdd failed @ iptMgr.Add")
	}

	newGaugeVal, err3 := promutil.GetValue(metrics.NumIPTableRules)
	newCountVal, err4 := promutil.GetCountValue(metrics.AddIPTableRuleExecTime)
	promutil.NotifyIfErrors(t, err1, err2, err3, err4)
	if newGaugeVal != gaugeVal+1 {
		t.Errorf("Change in iptable rule number didn't register in prometheus")
	}
	if newCountVal != countVal+1 {
		t.Errorf("Execution time didn't register in prometheus")
	}
}

func TestDelete(t *testing.T) {
	iptMgr := &IptablesManager{}
	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestDelete failed @ iptMgr.Save")
	}

	defer func() {
		if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestDelete failed @ iptMgr.Restore")
		}
	}()

	entry := &IptEntry{
		Chain: util.IptablesForwardChain,
		Specs: []string{
			util.IptablesJumpFlag,
			util.IptablesReject,
		},
	}
	if err := iptMgr.Add(entry); err != nil {
		t.Errorf("TestDelete failed @ iptMgr.Add")
	}

	gaugeVal, err1 := promutil.GetValue(metrics.NumIPTableRules)

	if err := iptMgr.Delete(entry); err != nil {
		t.Errorf("TestDelete failed @ iptMgr.Delete")
	}

	newGaugeVal, err2 := promutil.GetValue(metrics.NumIPTableRules)
	promutil.NotifyIfErrors(t, err1, err2)
	if newGaugeVal != gaugeVal-1 {
		t.Errorf("Change in iptable rule number didn't register in prometheus")
	}
}

func TestRun(t *testing.T) {
	iptMgr := &IptablesManager{}
	if err := iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestRun failed @ iptMgr.Save")
	}

	defer func() {
		if err := iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestRun failed @ iptMgr.Restore")
		}
	}()

	iptMgr.OperationFlag = util.IptablesChainCreationFlag
	entry := &IptEntry{
		Chain: "TEST-CHAIN",
	}
	if _, err := iptMgr.run(entry); err != nil {
		t.Errorf("TestRun failed @ iptMgr.run")
	}
}

func TestGetChainLineNumber(t *testing.T) {
	iptMgr := &IptablesManager{}

	var (
		lineNum    int
		err        error
		kubeExists bool
		npmExists  bool
	)

	if err = iptMgr.Save(util.IptablesTestConfigFile); err != nil {
		t.Errorf("TestGetChainLineNumber failed @ iptMgr.Save")
	}

	defer func() {
		if err = iptMgr.Restore(util.IptablesTestConfigFile); err != nil {
			t.Errorf("TestGetChainLineNumber failed @ iptMgr.Restore")
		}
	}()

	if err = iptMgr.addChain(util.IptablesKubeServicesChain); err != nil {
		t.Errorf("TestGetChainLineNumber failed @ kube-services chain iptMgr. addChain error: %s", err.Error())
	}

	iptMgr.OperationFlag = util.IptablesCheckFlag
	entry := &IptEntry{
		Chain: util.IptablesForwardChain,
		Specs: []string{
			util.IptablesJumpFlag,
			util.IptablesKubeServicesChain,
		},
	}

	if kubeExists, err = iptMgr.exists(entry); err != nil {
		t.Errorf("TestGetChainLineNumber failed @ kube-services chain iptMgr. exists error: %s", err.Error())
	}

	entry = &IptEntry{
		Chain: util.IptablesForwardChain,
		Specs: []string{
			util.IptablesJumpFlag,
			util.IptablesAzureChain,
		},
	}

	// Ignore not exists errors
	npmExists, _ = iptMgr.exists(entry)

	lineNum, err = iptMgr.getChainLineNumber(util.IptablesAzureChain, util.IptablesForwardChain)
	if err != nil {
		t.Errorf("TestGetChainLineNumber @ initial iptMgr.GetChainLineNumber error: %s", err.Error())
	}

	switch {
	case (npmExists && kubeExists):
		if lineNum != 3 {
			t.Errorf("TestGetChainLineNumber @ initial line number check iptMgr.GetChainLineNumber with npmExists: %t kubeExists: %t", npmExists, kubeExists)
		}
	case npmExists:
		if lineNum == 0 {
			t.Errorf("TestGetChainLineNumber @ initial line number check iptMgr.GetChainLineNumber with npmExists: %t kubeExists: %t", npmExists, kubeExists)
		}
	default:
		if lineNum != 0 {
			t.Errorf("TestGetChainLineNumber @ initial line number check iptMgr.GetChainLineNumber with npmExists: %t kubeExists: %t", npmExists, kubeExists)
		}
	}

	if err = iptMgr.InitNpmChains(); err != nil {
		t.Errorf("TestGetChainLineNumber @ iptMgr.InitNpmChains error: %s", err.Error())
	}

	entry = &IptEntry{
		Chain: util.IptablesForwardChain,
		Specs: []string{
			util.IptablesJumpFlag,
			util.IptablesAzureChain,
		},
	}

	if npmExists, err = iptMgr.exists(entry); err != nil {
		t.Errorf("TestGetChainLineNumber failed @ azure-npm chain iptMgr. exists error: %s", err.Error())
	}

	lineNum, err = iptMgr.getChainLineNumber(util.IptablesAzureChain, util.IptablesForwardChain)
	if err != nil {
		t.Errorf("TestGetChainLineNumber @ after Init chains iptMgr.GetChainLineNumber error: %s", err.Error())
	}

	switch {
	case (npmExists && kubeExists):
		if lineNum < 2 {
			t.Errorf("TestGetChainLineNumber @ after Init chains line number check iptMgr.GetChainLineNumber with npmExists: %t kubeExists: %t", npmExists, kubeExists)
		}
	case npmExists:
		if lineNum == 0 {
			t.Errorf("TestGetChainLineNumber @ after Init chains line number check iptMgr.GetChainLineNumber with npmExists: %t kubeExists: %t", npmExists, kubeExists)
		}
	case !npmExists:
		t.Errorf("TestGetChainLineNumber @ after Init chains line number check iptMgr.GetChainLineNumber with failed to Add chain ")
	}

	if err = iptMgr.UninitNpmChains(); err != nil {
		t.Errorf("TestGetChainLineNumber @ iptMgr.UninitNpmChains")
	}
}

func TestMain(m *testing.M) {
	metrics.InitializeAll()
	iptMgr := NewIptablesManager()
	iptMgr.Save(util.IptablesConfigFile)

	exitCode := m.Run()

	iptMgr.Restore(util.IptablesConfigFile)

	os.Exit(exitCode)
}
