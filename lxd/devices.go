package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	_ "github.com/mattn/go-sqlite3"

	"github.com/lxc/lxd/shared"
	"gopkg.in/lxc/go-lxc.v2"
)

func addBlockDev(dev string) ([]string, error) {
	stat := syscall.Stat_t{}
	err := syscall.Stat(dev, &stat)
	if err != nil {
		return []string{}, err
	}
	k := "lxc.cgroup.devices.allow"
	v := fmt.Sprintf("b %d:%d rwm", uint(stat.Rdev/256), uint(stat.Rdev%256))
	line := []string{k, v}
	return line, err
}

func DeviceToLxc(d shared.Device) ([][]string, error) {
	switch d["type"] {
	case "unix-char":
		return nil, fmt.Errorf("Not implemented")
	case "unix-block":
		return nil, fmt.Errorf("Not implemented")
	case "nic":
		if d["nictype"] != "bridged" && d["nictype"] != "" {
			return nil, fmt.Errorf("Bad nic type: %s\n", d["nictype"])
		}
		var l1 = []string{"lxc.network.type", "veth"}
		var lines = [][]string{l1}
		var l2 []string
		if d["hwaddr"] != "" {
			l2 = []string{"lxc.network.hwaddr", d["hwaddr"]}
			lines = append(lines, l2)
		}
		if d["mtu"] != "" {
			l2 = []string{"lxc.network.mtu", d["mtu"]}
			lines = append(lines, l2)
		}
		if d["parent"] != "" {
			l2 = []string{"lxc.network.link", d["parent"]}
			lines = append(lines, l2)
		}
		if d["name"] != "" {
			l2 = []string{"lxc.network.name", d["name"]}
			lines = append(lines, l2)
		}
		return lines, nil
	case "disk":
		var p string
		configLines := [][]string{}
		if d["path"] == "/" || d["path"] == "" {
			p = ""
		} else if d["path"][0:1] == "/" {
			p = d["path"][1:]
		} else {
			p = d["path"]
		}
		/* TODO - check whether source is a disk, loopback, btrfs subvol, etc */
		/* for now we only handle directory bind mounts */
		source := d["source"]
		fstype := "none"
		options := []string{}
		var err error
		if shared.IsBlockdevPath(d["source"]) {
			fstype, err = shared.BlockFsDetect(d["source"])
			if err != nil {
				return nil, fmt.Errorf("Error setting up %s: %s\n", d["name"], err)
			}
			l, err := addBlockDev(d["source"])
			if err != nil {
				return nil, fmt.Errorf("Error adding blockdev: %s\n", err)
			}
			configLines = append(configLines, l)
		} else if shared.IsDir(source) {
			options = append(options, "bind")
			options = append(options, "create=dir")
		} else /* file bind mount */ {
			/* Todo - can we distinguish between file bind mount and
			 * a qcow2 (or other fs container) file? */
			options = append(options, "bind")
			options = append(options, "create=file")
		}
		if d["readonly"] == "1" || d["readonly"] == "true" {
			options = append(options, "ro")
		}
		if d["optional"] == "1" || d["optional"] == "true" {
			options = append(options, "optional")
		}
		opts := strings.Join(options, ",")
		if opts == "" {
			opts = "defaults"
		}
		l := []string{"lxc.mount.entry", fmt.Sprintf("%s %s %s %s 0 0", source, p, fstype, opts)}
		configLines = append(configLines, l)
		return configLines, nil
	case "none":
		return nil, nil
	default:
		return nil, fmt.Errorf("Bad device type")
	}
}

func dbDeviceTypeToString(t int) (string, error) {
	switch t {
	case 0:
		return "none", nil
	case 1:
		return "nic", nil
	case 2:
		return "disk", nil
	case 3:
		return "unix-char", nil
	case 4:
		return "unix-block", nil
	default:
		return "", fmt.Errorf("Invalid device type %d\n", t)
	}
}

func DeviceTypeToDbType(t string) (int, error) {
	switch t {
	case "none":
		return 0, nil
	case "nic":
		return 1, nil
	case "disk":
		return 2, nil
	case "unix-char":
		return 3, nil
	case "unix-block":
		return 4, nil
	default:
		return -1, fmt.Errorf("Invalid device type %s\n", t)
	}
}

func ValidDeviceType(t string) bool {
	_, err := DeviceTypeToDbType(t)
	return err == nil
}

func ValidDeviceConfig(t, k, v string) bool {
	if k == "type" {
		return false
	}
	switch t {
	case "unix-char":
		switch k {
		case "path":
			return true
		case "major":
			return true
		case "minor":
			return true
		case "uid":
			return true
		case "gid":
			return true
		case "mode":
			return true
		default:
			return false
		}
	case "unix-block":
		switch k {
		case "path":
			return true
		case "major":
			return true
		case "minor":
			return true
		case "uid":
			return true
		case "gid":
			return true
		case "mode":
			return true
		default:
			return false
		}
	case "nic":
		switch k {
		case "parent":
			return true
		case "name":
			return true
		case "hwaddr":
			return true
		case "mtu":
			return true
		case "nictype":
			if v != "bridged" && v != "" {
				return false
			}
			return true
		default:
			return false
		}
	case "disk":
		switch k {
		case "path":
			return true
		case "source":
			return true
		case "readonly", "optional":
			return true
		default:
			return false
		}
	case "none":
		return false
	default:
		return false
	}
}

func AddDevices(tx *sql.Tx, w string, cID int, devices shared.Devices) error {
	str1 := fmt.Sprintf("INSERT INTO %ss_devices (%s_id, name, type) VALUES (?, ?, ?)", w, w)
	stmt1, err := tx.Prepare(str1)
	if err != nil {
		return err
	}
	defer stmt1.Close()
	str2 := fmt.Sprintf("INSERT INTO %ss_devices_config (%s_device_id, key, value) VALUES (?, ?, ?)", w, w)
	stmt2, err := tx.Prepare(str2)
	if err != nil {
		return err
	}
	defer stmt2.Close()
	for k, v := range devices {
		t, err := DeviceTypeToDbType(v["type"])
		if err != nil {
			return err
		}
		result, err := stmt1.Exec(cID, k, t)
		if err != nil {
			return err
		}
		id64, err := result.LastInsertId()
		if err != nil {
			return fmt.Errorf("Error inserting device %s into database", k)
		}
		// TODO: is this really int64? we should fix it everywhere if so
		id := int(id64)
		for ck, cv := range v {
			if ck == "type" {
				continue
			}
			if !ValidDeviceConfig(v["type"], ck, cv) {
				return fmt.Errorf("Invalid device config %s %s\n", ck, cv)
			}
			_, err = stmt2.Exec(id, ck, cv)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func tempNic() string {
	randBytes := make([]byte, 4)
	rand.Read(randBytes)
	return "lxd" + hex.EncodeToString(randBytes)
}

func inList(l []string, s string) bool {
	for _, ls := range l {
		if ls == s {
			return true
		}
	}
	return false
}

func nextUnusedNic(c *lxdContainer) string {
	list, err := c.c.Interfaces()
	if err != nil || len(list) == 0 {
		return "eth0"
	}
	i := 0
	// is it worth sorting list?
	for {
		nic := fmt.Sprintf("eth%d", i)
		if !inList(list, nic) {
			return nic
		}
		i = i + 1
	}
}

func setupNic(c *lxdContainer, d map[string]string) (string, error) {
	if d["nictype"] != "bridged" {
		return "", fmt.Errorf("Unsupported nic type: %s\n", d["nictype"])
	}
	if d["parent"] == "" {
		return "", fmt.Errorf("No bridge given\n")
	}
	if d["name"] == "" {
		d["name"] = nextUnusedNic(c)
	}

	n1 := tempNic()
	n2 := tempNic()

	err := exec.Command("ip", "link", "add", n1, "type", "veth", "peer", "name", n2).Run()
	if err != nil {
		return "", err
	}
	err = exec.Command("brctl", "addif", d["parent"], n1).Run()
	if err != nil {
		RemoveInterface(n2)
		return "", err
	}

	return n2, nil
}

func RemoveInterface(nic string) {
	_ = exec.Command("ip", "link", "del", nic).Run()
}

/*
 * Detach an interface in a container
 * The thing is, there doesn't seem to be a good way of doing
 * this without relying on /sys in the container or /sbin/ip
 * in the container being reliable.  We can look at the
 * /sys/devices/virtual/net/$name/ifindex (i.e. if 7, then delete 8 on host)
 * we can just ip link del $name in the container.
 *
 * if we just did a lxc config device add of this nic, then
 * lxc simply doesn't know the peername for this nic
 *
 * probably the thing to do is re-exec ourselves asking to
 * setns into the container's netns (only) and remove the nic.  for
 * now just don't do it, but don't fail either.
 */
func detachInterface(c *lxdContainer, key string) error {
	options := lxc.DefaultAttachOptions
	options.ClearEnv = false
	options.Namespaces = syscall.CLONE_NEWNET
	nullDev, err := os.OpenFile(os.DevNull, os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	defer nullDev.Close()
	nullfd := nullDev.Fd()
	options.StdinFd = nullfd
	options.StdoutFd = nullfd
	options.StderrFd = nullfd
	command := []string{"ip", "link", "del", key}
	_, err = c.c.RunCommand(command, options)
	return err
}

func txUpdateNic(tx *sql.Tx, cId int, devname string, nicname string) error {
	q := `
	SELECT id FROM containers_devices
	WHERE container_id == ? AND type == 1 AND name == ?`
	var dId int
	err := tx.QueryRow(q, cId, devname).Scan(&dId)
	if err != nil {
		return err
	}

	stmt := `INSERT into containers_devices_config (container_device_id, key, value) VALUES (?, ?, ?)`
	_, err = tx.Exec(stmt, dId, "name", nicname)
	return err
}

func (d *lxdContainer) detachMount(m shared.Device) error {
	// TODO - in case of reboot, we should remove the lxc.mount.entry.  Trick
	// is, we can't d.c.ClearConfigItem bc that will clear all the keys.  So
	// we should get the full list, clear, then reinsert all but the one we're
	// removing
	shared.Debugf("Mounts detach not yet implemented")

	pid := d.c.InitPid()
	if pid == -1 { // container not running
		return nil
	}
	pidstr := fmt.Sprintf("%d", pid)
	return exec.Command(os.Args[0], "forkumount", pidstr, m["path"]).Run()
}

func (d *lxdContainer) attachMount(m shared.Device) error {
	dest := m["path"]
	source := m["source"]

	opts := ""
	fstype := "none"
	flags := 0
	sb, err := os.Stat(source)
	if err != nil {
		return err
	}
	if sb.IsDir() {
		flags |= syscall.MS_BIND
		opts = "bind,create=dir"
	} else {
		if !shared.IsBlockdev(sb.Mode()) {
			// Not sure if we want to try dealing with loopdevs, but
			// since we'd need to deal with partitions i think not.
			// We also might want to support file bind mounting, but
			// this doesn't do that.
			return fmt.Errorf("non-block device file not supported\n")
		}

		fstype, err = shared.BlockFsDetect(source)
		if err != nil {
			return fmt.Errorf("Unable to detect fstype for %s: %s\n", source, err)
		}
	}

	// add a lxc.mount.entry = souce destination, in case of reboot
	if m["readonly"] == "1" || m["readonly"] == "true" {
		if opts == "" {
			opts = "ro"
		} else {
			opts = opts + ",ro"
		}
	}
	optional := false
	if m["optional"] == "1" || m["optional"] == "true" {
		optional = true
		opts = opts + ",optional"
	}

	entry := fmt.Sprintf("%s %s %s %s 0 0", source, dest, fstype, opts)
	if err := d.c.SetConfigItem("lxc.mount.entry", entry); err != nil {
		return err
	}

	pid := d.c.InitPid()
	if pid == -1 { // container not running - we're done
		return nil
	}

	// now live-mount
	tmpMount, err := ioutil.TempDir(shared.VarPath("shmounts", d.name), "lxdmount_")
	if err != nil {
		return err
	}

	err = syscall.Mount(m["source"], tmpMount, fstype, uintptr(flags), "")
	if err != nil {
		return err
	}

	mntsrc := filepath.Join("/.lxdmounts", filepath.Base(tmpMount))
	// finally we need to move-mount this in the container
	pidstr := fmt.Sprintf("%d", pid)
	err = exec.Command(os.Args[0], "forkmount", pidstr, mntsrc, m["path"]).Run()
	syscall.Unmount(tmpMount, syscall.MNT_DETACH) // in case forkmount failed
	os.Remove(tmpMount)

	if err != nil && !optional {
		return err
	}
	return nil
}

/*
 * Given a running container and a list of devices before and after a
 * config change, update the devices in the container.
 *
 * Currently we only support nics.  Disks will be supported once we
 * decide how best to insert them.
 */
func devicesApplyDeltaLive(tx *sql.Tx, c *lxdContainer, preDevList shared.Devices, postDevList shared.Devices) error {
	rmList, addList := preDevList.Update(postDevList)
	var err error

	// note - currently Devices.Update() only returns nics
	for key, dev := range rmList {
		switch dev["type"] {
		case "nic":
			if dev["name"] == "" {
				return fmt.Errorf("Do not know a name for the nic for device %s\n", key)
			}
			if err := detachInterface(c, dev["name"]); err != nil {
				return fmt.Errorf("Error removing device %s (nic %s) from container %s: %s", key, dev["name"], c.name, err)
			}
		case "disk":
			return c.detachMount(dev)
		}
	}

	for key, dev := range addList {
		switch dev["type"] {
		case "nic":
			var tmpName string
			if tmpName, err = setupNic(c, dev); err != nil {
				return fmt.Errorf("Unable to create nic %s for container %s: %s", dev["name"], c.name, err)
			}
			if err := c.c.AttachInterface(tmpName, dev["name"]); err != nil {
				RemoveInterface(tmpName)
				return fmt.Errorf("Unable to move nic %s into container %s as %s: %s", tmpName, c.name, dev["name"], err)
			}
			// Now we need to add the name to the database
			if err := txUpdateNic(tx, c.id, key, dev["name"]); err != nil {
				shared.Debugf("Warning: failed to update database entry for new nic %s: %s\n", key, err)
			}
		case "disk":
			if dev["source"] == "" || dev["path"] == "" {
				return fmt.Errorf("no source or destination given")
			}
			return c.attachMount(dev)
		}
	}

	return nil
}

func validateConfig(c *lxdContainer, devs shared.Devices) error {
	for _, dev := range devs {
		if dev["type"] == "disk" && shared.IsBlockdevPath(dev["source"]) {
			if !c.isPrivileged() {
				return fmt.Errorf("Only privileged containers may mount block devices")
			}
		}
	}
	return nil
}
