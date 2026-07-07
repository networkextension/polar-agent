package hostinfo

import "testing"

func TestParseWindowsCIMJSON(t *testing.T) {
	blob := `{"uuid":"C5A2B3D4-0000-11EE-8000-ABCDEF012345","vendor":"LENOVO","model":"21LSCTO1WW","model_name":"ThinkPad T14s Gen 5","cpu":"Intel(R) Core(TM) Ultra 7 155U","cores":14,"mem":34093500416,"os":"Microsoft Windows 11 Pro","ver":"10.0.26200","build":"26200","boot":1751500000,"disk":511343247360,"gpu":"Intel(R) Graphics","gpu_vendor":"Intel Corporation","battery":true}`
	var h HostInfo
	parseWindowsCIMJSON(&h, blob+"\r\n")
	if h.MachineUUID != "C5A2B3D4-0000-11EE-8000-ABCDEF012345" {
		t.Errorf("uuid = %q", h.MachineUUID)
	}
	if h.HwVendor != "LENOVO" || h.HwModel != "21LSCTO1WW" || h.ModelName != "ThinkPad T14s Gen 5" {
		t.Errorf("hw = %q %q %q", h.HwVendor, h.HwModel, h.ModelName)
	}
	if h.CPUCores != 14 || h.MemoryBytes != 34093500416 || h.DiskTotalBytes != 511343247360 {
		t.Errorf("shape = %d cores %d mem %d disk", h.CPUCores, h.MemoryBytes, h.DiskTotalBytes)
	}
	if h.OSName != "Microsoft Windows 11 Pro" || h.OSVersion != "10.0.26200" || h.OSBuild != "26200" {
		t.Errorf("os = %q %q %q", h.OSName, h.OSVersion, h.OSBuild)
	}
	if h.BootUnix != 1751500000 {
		t.Errorf("boot = %d", h.BootUnix)
	}
	if h.GPU == nil || h.GPU.Model != "Intel(R) Graphics" || h.GPU.Vendor != "Intel Corporation" {
		t.Errorf("gpu = %+v", h.GPU)
	}
	if h.HasBattery == nil || !*h.HasBattery {
		t.Errorf("battery = %v", h.HasBattery)
	}
}

func TestParseWindowsCIMJSON_GarbageAndSparse(t *testing.T) {
	var h HostInfo
	parseWindowsCIMJSON(&h, "")              // powershell missing
	parseWindowsCIMJSON(&h, "At line:1 ...") // PS error text
	if h.MachineUUID != "" || h.GPU != nil || h.HasBattery != nil {
		t.Errorf("garbage input mutated HostInfo: %+v", h)
	}
	// Sparse object (every provider failed): nothing set, no panic.
	parseWindowsCIMJSON(&h, `{"uuid":null,"boot":0,"battery":null}`)
	if h.MachineUUID != "" || h.BootUnix != 0 || h.HasBattery != nil {
		t.Errorf("sparse input mutated HostInfo: %+v", h)
	}
}

func TestParseWindowsMachineGuid(t *testing.T) {
	blob := "\r\nHKEY_LOCAL_MACHINE\\SOFTWARE\\Microsoft\\Cryptography\r\n    MachineGuid    REG_SZ    3f0c8e21-9d51-4b7a-a0e5-6f2d9c184b77\r\n\r\n"
	if got := parseWindowsMachineGuid(blob); got != "3f0c8e21-9d51-4b7a-a0e5-6f2d9c184b77" {
		t.Errorf("got %q", got)
	}
	if got := parseWindowsMachineGuid("ERROR: The system was unable to find the specified registry key or value."); got != "" {
		t.Errorf("error text parsed to %q", got)
	}
}
