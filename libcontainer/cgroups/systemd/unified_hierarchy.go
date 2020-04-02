// +build linux

package systemd

import (
	"bytes"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	systemdDbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fs2"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type UnifiedManager struct {
	mu      sync.Mutex
	Cgroups *configs.Cgroup
	Paths   map[string]string
}

func (m *UnifiedManager) Apply(pid int) error {
	var (
		c          = m.Cgroups
		unitName   = getUnitName(c)
		slice      = "system.slice"
		properties []systemdDbus.Property
	)

	if c.Paths != nil {
		paths := make(map[string]string)
		for name, path := range c.Paths {
			paths[name] = path
		}
		m.Paths = paths
		return cgroups.EnterPid(m.Paths, pid)
	}

	if c.Parent != "" {
		slice = c.Parent
	}

	properties = append(properties, systemdDbus.PropDescription("libcontainer container "+c.Name))

	// if we create a slice, the parent is defined via a Wants=
	if strings.HasSuffix(unitName, ".slice") {
		properties = append(properties, systemdDbus.PropWants(slice))
	} else {
		// otherwise, we use Slice=
		properties = append(properties, systemdDbus.PropSlice(slice))
	}

	// only add pid if its valid, -1 is used w/ general slice creation.
	if pid != -1 {
		properties = append(properties, newProp("PIDs", []uint32{uint32(pid)}))
	}

	// Check if we can delegate. This is only supported on systemd versions 218 and above.
	if !strings.HasSuffix(unitName, ".slice") {
		// Assume scopes always support delegation.
		properties = append(properties, newProp("Delegate", true))
	}

	// Always enable accounting, this gets us the same behaviour as the fs implementation,
	// plus the kernel has some problems with joining the memory cgroup at a later time.
	properties = append(properties,
		newProp("MemoryAccounting", true),
		newProp("CPUAccounting", true),
		newProp("IOAccounting", true))

	// Assume DefaultDependencies= will always work (the check for it was previously broken.)
	properties = append(properties,
		newProp("DefaultDependencies", false))

	if c.Resources.Memory != 0 {
		properties = append(properties,
			newProp("MemoryMax", uint64(c.Resources.Memory)))
	}

	if c.Resources.CpuWeight != 0 {
		properties = append(properties,
			newProp("CPUWeight", c.Resources.CpuWeight))
	}

	// cpu.cfs_quota_us and cpu.cfs_period_us are controlled by systemd.
	if c.Resources.CpuQuota != 0 && c.Resources.CpuPeriod != 0 {
		// corresponds to USEC_INFINITY in systemd
		// if USEC_INFINITY is provided, CPUQuota is left unbound by systemd
		// always setting a property value ensures we can apply a quota and remove it later
		cpuQuotaPerSecUSec := uint64(math.MaxUint64)
		if c.Resources.CpuQuota > 0 {
			// systemd converts CPUQuotaPerSecUSec (microseconds per CPU second) to CPUQuota
			// (integer percentage of CPU) internally.  This means that if a fractional percent of
			// CPU is indicated by Resources.CpuQuota, we need to round up to the nearest
			// 10ms (1% of a second) such that child cgroups can set the cpu.cfs_quota_us they expect.
			cpuQuotaPerSecUSec = uint64(c.Resources.CpuQuota*1000000) / c.Resources.CpuPeriod
			if cpuQuotaPerSecUSec%10000 != 0 {
				cpuQuotaPerSecUSec = ((cpuQuotaPerSecUSec / 10000) + 1) * 10000
			}
		}
		properties = append(properties,
			newProp("CPUQuotaPerSecUSec", cpuQuotaPerSecUSec))
	}

	if c.Resources.PidsLimit > 0 {
		properties = append(properties,
			newProp("TasksAccounting", true),
			newProp("TasksMax", uint64(c.Resources.PidsLimit)))
	}

	properties = append(properties, c.SystemdProps...)

	// ignore c.Resources.KernelMemory

	dbusConnection, err := getDbusConnection()
	if err != nil {
		return err
	}

	statusChan := make(chan string, 1)
	if _, err := dbusConnection.StartTransientUnit(unitName, "replace", properties, statusChan); err == nil {
		select {
		case <-statusChan:
		case <-time.After(time.Second):
			logrus.Warnf("Timed out while waiting for StartTransientUnit(%s) completion signal from dbus. Continuing...", unitName)
		}
	} else if !isUnitExists(err) {
		return err
	}

	path, err := getv2Path(m.Cgroups)
	if err != nil {
		return err
	}
	if err := createCgroupsv2Path(path); err != nil {
		return err
	}
	m.Paths = map[string]string{
		"pids":    path,
		"memory":  path,
		"io":      path,
		"cpu":     path,
		"devices": path,
		"cpuset":  path,
		"freezer": path,
	}
	return nil
}

func (m *UnifiedManager) Destroy() error {
	if m.Cgroups.Paths != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	dbusConnection, err := getDbusConnection()
	if err != nil {
		return err
	}
	dbusConnection.StopUnit(getUnitName(m.Cgroups), "replace", nil)
	if err := cgroups.RemovePaths(m.Paths); err != nil {
		return err
	}
	m.Paths = make(map[string]string)
	return nil
}

func (m *UnifiedManager) GetPaths() map[string]string {
	m.mu.Lock()
	paths := m.Paths
	m.mu.Unlock()
	return paths
}
func (m *UnifiedManager) GetUnifiedPath() (string, error) {
	unifiedPath := ""
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.Paths {
		if unifiedPath == "" {
			unifiedPath = v
		} else if v != unifiedPath {
			return unifiedPath,
				errors.Errorf("expected %q path to be unified path %q, got %q", k, unifiedPath, v)
		}
	}
	if unifiedPath == "" {
		// FIXME: unified path could be detected even when no controller is available
		return unifiedPath, errors.New("cannot detect unified path")
	}
	return unifiedPath, nil
}

func getv2Path(c *configs.Cgroup) (string, error) {
	slice := "system.slice"
	if c.Parent != "" {
		slice = c.Parent
	}

	slice, err := ExpandSlice(slice)
	if err != nil {
		return "", err
	}

	return filepath.Join(fs2.UnifiedMountpoint, slice, getUnitName(c)), nil
}

func createCgroupsv2Path(path string) (Err error) {
	content, err := ioutil.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return err
	}

	ctrs := bytes.Fields(content)
	res := append([]byte("+"), bytes.Join(ctrs, []byte(" +"))...)

	current := "/sys/fs"
	elements := strings.Split(path, "/")
	for i, e := range elements[3:] {
		current = filepath.Join(current, e)
		if i > 0 {
			if err := os.Mkdir(current, 0755); err != nil {
				if !os.IsExist(err) {
					return err
				}
			} else {
				// If the directory was created, be sure it is not left around on errors.
				defer func() {
					if Err != nil {
						os.Remove(current)
					}
				}()
			}
		}
		if i < len(elements[3:])-1 {
			if err := ioutil.WriteFile(filepath.Join(current, "cgroup.subtree_control"), res, 0755); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *UnifiedManager) fsManager() (cgroups.Manager, error) {
	path, err := m.GetUnifiedPath()
	if err != nil {
		return nil, err
	}
	return fs2.NewManager(m.Cgroups, path, false)
}

func (m *UnifiedManager) Freeze(state configs.FreezerState) error {
	fsMgr, err := m.fsManager()
	if err != nil {
		return err
	}
	return fsMgr.Freeze(state)
}

func (m *UnifiedManager) GetPids() ([]int, error) {
	path, err := m.GetUnifiedPath()
	if err != nil {
		return nil, err
	}
	return cgroups.GetPids(path)
}

func (m *UnifiedManager) GetAllPids() ([]int, error) {
	path, err := m.GetUnifiedPath()
	if err != nil {
		return nil, err
	}
	return cgroups.GetAllPids(path)
}

func (m *UnifiedManager) GetStats() (*cgroups.Stats, error) {
	fsMgr, err := m.fsManager()
	if err != nil {
		return nil, err
	}
	return fsMgr.GetStats()
}

func (m *UnifiedManager) Set(container *configs.Config) error {
	fsMgr, err := m.fsManager()
	if err != nil {
		return err
	}
	return fsMgr.Set(container)
}

func (m *UnifiedManager) GetCgroups() (*configs.Cgroup, error) {
	return m.Cgroups, nil
}