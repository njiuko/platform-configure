// +build linux,amd64

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"github.com/coreos/go-systemd/dbus"
	"github.com/experimental-platform/platform-utils/netutil"
	"github.com/vishvananda/netlink"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"text/template"
)

const reportTemplate = `
Interface: {{ .Interface }}
=====================
Mode:      {{ .Mode }}
State:     {{ .State }}
Associated Addresses:
{{ range $key, $value := .Addresses }}    * {{ $value }}
{{ else }}    -- NONE --
{{ end }}HWAddress:   {{ .HWAddress }}
Gateway:   {{ .Gateway }}
Nameserver:
{{ range $key, $value := .Nameserver }}    * {{ $value }}
{{ end }}Domains:
{{ range $key, $value := .Domains }}    * {{ $value }}
{{ end }}`

type reportTemplateData struct {
	Interface  string
	State      string
	Mode       string
	Addresses  []string
	HWAddress  string
	Gateway    string
	Domains    []string
	Nameserver []string
}

type NetLink interface {
	LinkByName(string) (netlink.Link, error)
	RouteList(netlink.Link, int) ([]netlink.Route, error)
	AddrList(netlink.Link, int) ([]netlink.Addr, error)
	LinkList() ([]netlink.Link, error)
}

type realNL struct {
}

func (n realNL) LinkByName(s string) (netlink.Link, error) {
	return netlink.LinkByName(s)
}

func (n realNL) RouteList(l netlink.Link, f int) ([]netlink.Route, error) {
	return netlink.RouteList(l, f)
}

func (n realNL) AddrList(l netlink.Link, f int) ([]netlink.Addr, error) {
	return netlink.AddrList(l, f)
}

func (n realNL) LinkList() ([]netlink.Link, error) {
	return netlink.LinkList()
}

// make sure realNL satisfies the NetLink interface
var _ NetLink = (*realNL)(nil)

var nl NetLink = realNL{}

type NetUtil interface {
	GetInterfaceStats(string) (netutil.InterfaceData, error)
}

type realNU struct {
}

func (n realNU) GetInterfaceStats(name string) (netutil.InterfaceData, error) {
	return netutil.GetInterfaceStats(name)
}

// make sure realNU satisfies the NetUtil interface
var _ NetUtil = (*realNU)(nil)

var nu NetUtil = realNU{}

func getNetInterfaceData(name string) (*reportTemplateData, error) {
	result := new(reportTemplateData)
	result.Interface = name
	interfaceData, err := nu.GetInterfaceStats(result.Interface)
	if err != nil {
		return result, err
	}
	if interfaceData.NETWORK_FILE == "/usr/lib64/systemd/network/zz-default.network" {
		result.Mode = "DHCP"
	} else {
		result.Mode = "STATIC"
	}
	result.Nameserver = interfaceData.DNS
	result.Domains = interfaceData.DOMAINS
	result.State = interfaceData.OPER_STATE
	link, err := nl.LinkByName(result.Interface)
	if err != nil {
		return result, err
	}
	// add hardware address
	linkAttrs := link.Attrs()
	result.HWAddress = linkAttrs.HardwareAddr.String()
	// add ip addresses and netmasks
	addrList, err := nl.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return result, err
	}
	for _, addr := range addrList {
		result.Addresses = append(result.Addresses, fmt.Sprintf("%s", addr.IPNet))
	}
	// Add Gateway
	routeList, err := nl.RouteList(link, netlink.FAMILY_V4)
	if err != nil && len(routeList) < 1 {
		return result, err
	}
	for _, route := range routeList {
		if route.Gw != nil {
			result.Gateway = route.Gw.String()
			break
		}
	}
	return result, nil
}

func reportOnInterface(name string) (string, error) {
	result, err := getNetInterfaceData(name)
	if err != nil {
		return "", err
	}
	// Create the report
	report, err := template.New("Report").Parse(reportTemplate)
	if err != nil {
		return "", err
	}
	buff := bytes.NewBufferString("")
	err = report.Execute(buff, result)
	if err != nil {
		return "", err
	}
	return buff.String(), nil
}

func listInterfaces() ([]string, error) {
	result := []string{}
	linkList, err := nl.LinkList()
	if err != nil {
		return result, err
	}
	for _, entry := range linkList {
		attrs := entry.Attrs()
		// all hardware interfaces (NOT their aliases) have a TxQLen > 1
		if attrs.TxQLen > 1 && !strings.HasPrefix(attrs.Name, "wl") {
			result = append(result, attrs.Name)
		}
	}
	return result, err
}

func ShowConfig() (string, error) {
	// TODO: test this!
	var err error
	var result string
	interfaceNames, err := listInterfaces()
	if err != nil {
		return result, err
	}
	for _, name := range interfaceNames {
		report, err := reportOnInterface(name)
		if err != nil {
			return "", err
		}
		result += report
	}
	return result, err
}

func restartNetworkD() error {
	var connection, err = dbus.New()
	if err != nil {
		return err
	}
	result_channel := make(chan string, 1)
	var result string
	_, err = connection.RestartUnit("systemd-networkd.service", "fail", result_channel)
	if err == nil {
		result = <-result_channel
	} else {
		// Systemd Unit ERROR
		return err
	}
	if result != "done" {
		return fmt.Errorf("Unexpected SYSTEMD API result: %s", result)
	}
	return nil
}

func EnableDHCP(name string) (string, error) {
	// TODO: Test this!
	result := fmt.Sprintf("Getting interface stats for '%s'...", name)
	iData, err := nu.GetInterfaceStats(name)
	if err != nil {
		return result, fmt.Errorf("\nERROR: Interface '%s' not found.", name)
	}
	result += "OKAY\n"
	if iData.NETWORK_FILE == "/usr/lib64/systemd/network/zz-default.network" {
		result += "SUCCESS: Already using DHCP\n"
		return result, nil
	} else {
		// in most cases we'll just remove any user provided config, so the systems default takes hold
		if strings.Contains(iData.NETWORK_FILE, "/etc/systemd/network/") {
			result += fmt.Sprintf("Custon Config detected: '%s'\n", iData.NETWORK_FILE)
			if _, err := os.Stat(iData.NETWORK_FILE); err == nil {
				// It's okay if the file does not exists, as this frequently happens
				// when configuring stuff manually or when not rebooting.
				err = os.Remove(iData.NETWORK_FILE)
				if err != nil {
					return result, fmt.Errorf("ERROR removing '%s'\n", iData.NETWORK_FILE)
				}

			}
			result += fmt.Sprintf("Successfully removed '%s'\n", iData.NETWORK_FILE)
			err = restartNetworkD()
			if err != nil {
				result += "SUCCESS, PLEASE REBOOT!\n"
			}
			result += "SUCCESS, using DHCP.\nPLEASE REBOOT NOW ('sudo reboot')!\n"
			return result, nil
		} else {
			return result, fmt.Errorf("Sorry, no idea how to handle '%s'.", iData.NETWORK_FILE)
		}
	}
}

type staticData struct {
	Mac     string
	Address string
	Gateway string
	DNS     []string
}

const unitTemplate = `[Match]
MACAddress={{ .Mac }}

[Network]
Address={{ .Address }}
Gateway={{ .Gateway }}
{{ range $key, $value := .DNS }}DNS={{ $value }}
{{ end }}`

func parseMap(s string) (*net.IPMask, error) {
	r := make(net.IPMask, net.IPv4len)
	o := strings.Split(s, ".")
	if len(o) != 4 {
		return &r, fmt.Errorf("Not a valid net mask: %s", s)
	}
	for i, s := range o {
		b, err := strconv.Atoi(s)
		if err != nil {
			return &r, err
		}
		r[i] = byte(b)
	}
	return &r, nil
}

func SetStaticConfig(iface, address, netmask, gateway, dns string, nl NetLink) (string, error) {
	// TODO: test this!
	iData, err := nu.GetInterfaceStats(iface)
	result := fmt.Sprintf("Configuring interface '%v'...\n", iface)
	if err != nil {
		return result, fmt.Errorf("\nERROR: Interface '%s' not found.", iface)
	}
	templateData := new(staticData)
	// get the mac address
	link, err := nl.LinkByName(iface)
	if err != nil {
		return "", err
	}
	linkAttrs := link.Attrs()
	templateData.Mac = linkAttrs.HardwareAddr.String()
	// create IP/MASK entry
	mask, err := parseMap(netmask)
	if err != nil {
		return "", err
	}
	ipAddress := net.ParseIP(address)
	if ipAddress == nil {
		return "", fmt.Errorf("'%s' (address) is not a valid IP address.", address)
	}
	ipNet := net.IPNet{IP: ipAddress, Mask: *mask}
	templateData.Address = ipNet.String()
	gatewayIP := net.ParseIP(gateway)
	if !ipNet.Contains(gatewayIP) {
		return "", fmt.Errorf("🖕\tGateway address '%s' is not within the network '%s'.", gatewayIP, ipNet.String())
	}
	if gatewayIP == nil {
		return "", fmt.Errorf("'%s' (gateway) is not a valid IP address.", gateway)
	}
	templateData.Gateway = gatewayIP.String()
	// parse dns
	var dnsList []string
	for _, s := range strings.Split(strings.Trim(dns, " "), ",") {
		ip := net.ParseIP(strings.Trim(s, " "))
		if ip == nil {
			return "", fmt.Errorf("'%s' (dns) is not a valid IP address.", s)
		}
		dnsList = append(dnsList, ip.String())
	}
	templateData.DNS = dnsList

	unitData, err := template.New("Unit").Parse(unitTemplate)
	if err != nil {
		return "", err
	}
	buff := bytes.NewBufferString("")
	err = unitData.Execute(buff, templateData)
	if err != nil {
		return "", err
	}

	if iData.NETWORK_FILE != "/usr/lib64/systemd/network/zz-default.network" {
		if strings.Contains(iData.NETWORK_FILE, "/etc/systemd/network/") {
			err = os.Remove(iData.NETWORK_FILE)
			if err != nil {
				return "", err
			}
		} else {
			return "", fmt.Errorf("No idea what to do with '%s', sorry!", iData.NETWORK_FILE)
		}
	}
	err = ioutil.WriteFile(path.Join("/etc/systemd/network/", iface + ".network"), buff.Bytes(), 0644)
	if err != nil {
		return "", err
	}
	err = restartNetworkD()
	if err != nil {
		result += "SUCCESS, PLEASE REBOOT NOW (with 'sudo reboot')!\n"
	}
	result += "SUCCESS, using static configuration now!\n" +
		"PLEASE REBOOT NOW WITH 'sudo reboot'.\n"
	return result, err
}

func ResetToDHCP() (string, error) {
	interfaceNames, err := listInterfaces()
	var result string
	if err != nil {
		return "", err
	}
	for _, name := range interfaceNames {
		result += fmt.Sprintf("Resetting interface '%s':\n", name)
		message, err := EnableDHCP(name)
		result += fmt.Sprintf("%s\n\n", message)
		if err != nil {
			return result, err
		}
	}
	result += "\nSUCCESS.\n\nPLEASE REBOOT NOW ('sudo reboot')!\n"
	return result, err
}

func switchByCommandline() (string, error) {
	var CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	CommandLine.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		CommandLine.PrintDefaults()
	}
	show := CommandLine.Bool("show", false, "Show configuration and available interfaces")
	reset_all := CommandLine.Bool("reset", false, "(Re)set all interfaces to DHCP")
	mode := CommandLine.String("mode", "", "'dhcp' or 'static'")
	networkInterface := CommandLine.String("interface", "", "Interface name to be configured")
	address := CommandLine.String("address", "", "IP address to be set for the interface")
	netmask := CommandLine.String("netmask", "", "Set the netmask")
	gateway := CommandLine.String("gateway", "", "Gateway address")
	dns := CommandLine.String("dns", "", "IP addresses of DNS servers, separated by comma")
	version := CommandLine.Bool("version", false, "Show the version of this tool.")
	err := CommandLine.Parse(os.Args[1:])
	if err != nil {
		log.Fatalf("ERROR: %v", err)
	}
	switch {
	case *show:
		return ShowConfig()
	case *version:
		return "Version: 0.9 (missing menu and repair)", nil
	case *reset_all:
		return ResetToDHCP()
	case *mode == "dhcp":
		return EnableDHCP(*networkInterface)
	case *mode == "static":
		return SetStaticConfig(*networkInterface, *address, *netmask, *gateway, *dns, nl)
	default:
		// TODO: implement menu interface
		CommandLine.Usage()
		return "", errors.New("Invalid flag.")
	}
	return "Config done.", nil
}

func main() {
	message, err := switchByCommandline()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(message)
	os.Exit(0)
}