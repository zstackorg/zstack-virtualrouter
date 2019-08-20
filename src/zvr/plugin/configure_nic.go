package plugin

import (
	"zvr/server"
	"zvr/utils"
	"fmt"
	"github.com/pkg/errors"
	"strings"
	//log "github.com/Sirupsen/logrus"
)

const (
	VR_CONFIGURE_NIC = "/configurenic"
	VR_CONFIGURE_NIC_FIREWALL_DEFAULT_ACTION_PATH = "/configurenicdefaultaction";
	VR_REMOVE_NIC_PATH = "/removenic"
	ROUTE_STATE_NEW_ENABLE_FIREWALL_RULE_NUMBER = 9999
)

type nicInfo struct {
	Ip string `json:"ip"`
	Netmask string `json:"netmask"`
	Gateway string `json:"gateway"`
	Mac string `json:"Mac"`
	Category string `json:"category"`
	L2Type string `json:"l2type"`
	PhysicalInterface string `json:"physicalInterface"`
	Vni int `json:"vni"`
	FirewallDefaultAction string `json:"firewallDefaultAction"`
}

type configureNicCmd struct {
	Nics []nicInfo `json:"nics"`
}

func makeNicFirewallDescription(nicname, ip string) string {
	return fmt.Sprintf("nic-%s-secondary-ip-%s", nicname, ip)
}

func addSecondaryIpFirewall(nicname, ip string,  tree *server.VyosConfigTree)  {
	if (utils.IsSkipVyosIptables()) {
		rule := utils.NewIptablesRule("", "", ip, 0, 0, []string{utils.RELATED, utils.ESTABLISHED}, utils.ACCEPT, utils.VRRPComment)
		utils.InsertFireWallRule(nicname, rule, utils.LOCAL)
		rule = utils.NewIptablesRule("icmp", "", ip, 0, 0, nil, utils.ACCEPT, utils.VRRPComment)
		utils.InsertFireWallRule(nicname, rule, utils.LOCAL)
	} else {
		des := makeNicFirewallDescription(nicname, ip)
		if r := tree.FindFirewallRuleByDescription(nicname, "local", des); r == nil {
			tree.SetFirewallOnInterface(nicname, "local",
				fmt.Sprintf("description %s", des),
				"action accept",
				"state established enable",
				"state related enable",
				fmt.Sprintf("destination address %s", ip),
			)

			tree.SetFirewallOnInterface(nicname, "local",
				fmt.Sprintf("description %s", des),
				"action accept",
				"protocol icmp",
				fmt.Sprintf("destination address %s", ip),
			)
		}
		tree.AttachFirewallToInterface(nicname, "local")
	}

}

func configureNic(ctx *server.CommandContext) interface{} {
	cmd := &configureNicCmd{}
	ctx.GetCommand(cmd)

	tree := server.NewParserFromShowConfiguration().Tree
	var nicname string
	for _, nic := range cmd.Nics {
		err := utils.Retry(func() error {
			var e error
			nicname, e = utils.GetNicNameByMac(nic.Mac)
			if e != nil {
				return e
			} else {
				return nil
			}
		}, 5, 1); utils.PanicOnError(err)
		cidr, err := utils.NetmaskToCIDR(nic.Netmask); utils.PanicOnError(err)
		addr := fmt.Sprintf("%v/%v", nic.Ip, cidr)
		tree.SetfWithoutCheckExisting("interfaces ethernet %s address %v", nicname, addr)
		tree.SetfWithoutCheckExisting("interfaces ethernet %s duplex auto", nicname)
		tree.SetfWithoutCheckExisting("interfaces ethernet %s smp_affinity auto", nicname)
		tree.SetfWithoutCheckExisting("interfaces ethernet %s speed auto", nicname)

		if utils.IsSkipVyosIptables() {
			if nic.Category == "Private" {
				utils.InitNicFirewall(nicname, nic.Ip, false, utils.REJECT)
			} else {
				utils.InitNicFirewall(nicname, nic.Ip, true, utils.REJECT)
			}
		} else {
			tree.SetFirewallOnInterface(nicname, "local",
				"action accept",
				"state established enable",
				"state related enable",
				fmt.Sprintf("destination address %v", nic.Ip),
			)
			tree.SetFirewallOnInterface(nicname, "local",
				"action accept",
				"protocol icmp",
				fmt.Sprintf("destination address %v", nic.Ip),
			)

			if nic.Category == "Private" {
				tree.SetZStackFirewallRuleOnInterface(nicname, "behind","in",
					"action accept",
					"state established enable",
					"state related enable",
					"state invalid enable",
					"state new enable",
				)
			} else {
				tree.SetZStackFirewallRuleOnInterface(nicname, "behind","in",
					"action accept",
					"state established enable",
					"state related enable",
				)
			}

			tree.SetZStackFirewallRuleOnInterface(nicname, "behind","in",
				"action accept",
				"protocol icmp",
			)

			tree.SetFirewallWithRuleNumber(nicname, "in", ROUTE_STATE_NEW_ENABLE_FIREWALL_RULE_NUMBER,
				"action accept",
				"state new enable",
			)

			// only allow ssh traffic on eth0, disable on others
			if nicname == "eth0" {
				tree.SetFirewallOnInterface(nicname, "local",
					fmt.Sprintf("destination port %v", int(utils.GetSshPortFromBootInfo())),
					fmt.Sprintf("destination address %v", nic.Ip),
					"protocol tcp",
					"action accept",
				)
			} else {
				tree.SetFirewallOnInterface(nicname, "local",
					fmt.Sprintf("destination port %v", int(utils.GetSshPortFromBootInfo())),
					fmt.Sprintf("destination address %v", nic.Ip),
					"protocol tcp",
					"action reject",
				)
			}

			tree.SetFirewallDefaultAction(nicname, "local", "reject")
			tree.SetFirewallDefaultAction(nicname, "in", "reject")

			tree.AttachFirewallToInterface(nicname, "local")
			tree.AttachFirewallToInterface(nicname, "in")
		}

		if nic.L2Type != "" {
			b := utils.NewBash()
			b.Command = fmt.Sprintf("ip link set dev %s alias '%s'", nicname, makeAlias(nic))
			b.Run()
		}
	}

	tree.Apply(false)
	if (IsMaster()) {
		checkNicIsUp(nicname, true)
	} else {
		cmds := []string{}
		for _, nic := range cmd.Nics {
			nicname, _ = utils.GetNicNameByMac(nic.Mac)
			cmds = append(cmds, fmt.Sprintf("ip link set dev %v down", nicname))
		}
		b := utils.Bash{
			Command: strings.Join(cmds, "\n"),
		}

		b.Run()
		b.PanicIfError()
	}

	generateNotityScripts()
	/*
	vyosNics := []nicVipPair{}
	for _, nic := range cmd.Nics {
		if nic.SecondaryIps == nil {
			continue
		}

		nicname, _ = utils.GetNicNameByMac(nic.Mac)
		cidr, err := utils.NetmaskToCIDR(nic.Netmask); utils.PanicOnError(err)
		for _, sip := range nic.SecondaryIps {
			vyosNics = append(vyosNics, nicVipPair{NicName:nicname, Vip:sip, Prefix: cidr})
		}
	}
	addHaNicVipPair(vyosNics)*/

	return nil
}

func checkNicIsUp(nicname string, panicIfDown bool) error {
	var retryTimes uint = 10
	var retryInterval uint = 1

	bash := utils.Bash{
		Command:fmt.Sprintf("ip link show dev %s up", nicname),
	}
	err := utils.Retry(func() error {
		_, o, _, _ := bash.RunWithReturn()
		if o == "" {
			return errors.New(fmt.Sprintf("nic %s is down", nicname))
		} else {
			return nil
		}
	}, retryTimes, retryInterval)
	error := errors.New(fmt.Sprintf("nic %s still down after %d secondes", nicname, retryTimes * retryInterval))

	if err != nil && panicIfDown == true {
		utils.PanicOnError(error)
	} else if err != nil {
		return error
	}

	return nil
}

func removeNic(ctx *server.CommandContext) interface{} {
	cmd := &configureNicCmd{}
	ctx.GetCommand(cmd)

	tree := server.NewParserFromShowConfiguration().Tree
	for _, nic := range cmd.Nics {
		var nicname string
		err := utils.Retry(func() error {
			var e error
			nicname, e = utils.GetNicNameByMac(nic.Mac)
			if e != nil {
				return e
			} else {
				return nil
			}
		}, 5, 1); utils.PanicOnError(err)
		tree.Deletef("interfaces ethernet %s", nicname)
		if utils.IsSkipVyosIptables() {
			utils.DestroyNicFirewall(nicname)
		} else {
			tree.Deletef("firewall name %s.in", nicname)
			tree.Deletef("firewall name %s.local", nicname)
		}
	}
	tree.Apply(false)

	generateNotityScripts()
	/*
	vyosNics := []nicVipPair{}
	for _, nic := range cmd.Nics {
		if nic.SecondaryIps == nil {
			continue
		}

		nicname, _ := utils.GetNicNameByMac(nic.Mac)
		cidr, err := utils.NetmaskToCIDR(nic.Netmask); utils.PanicOnError(err)
		for _, sip := range nic.SecondaryIps {
			vyosNics = append(vyosNics, nicVipPair{NicName:nicname, Vip:sip, Prefix:cidr})
		}
	}
	removeHaNicVipPair(vyosNics)*/

	return nil
}

func configureNicFirewallDefaultAction(ctx *server.CommandContext) interface{} {
	cmd := &configureNicCmd{}
	ctx.GetCommand(cmd)

	tree := server.NewParserFromShowConfiguration().Tree
	var nicname string
	for _, nic := range cmd.Nics {
		err := utils.Retry(func() error {
			var e error
			nicname, e = utils.GetNicNameByMac(nic.Mac)
			if e != nil {
				return e
			} else {
				return nil
			}
		}, 5, 1); utils.PanicOnError(err)

		if utils.IsSkipVyosIptables() {
			utils.SetDefaultRule(nicname, nic.FirewallDefaultAction)
		} else {
			if (strings.Compare(nic.FirewallDefaultAction, "reject") == 0) {
				tree.SetFirewallDefaultAction(nicname, "local", "reject")
				tree.SetFirewallDefaultAction(nicname, "in", "reject")
			} else {
				tree.SetFirewallDefaultAction(nicname, "local", "accept")
				tree.SetFirewallDefaultAction(nicname, "in", "accept")
			}
		}
	}

	tree.Apply(false)
	return nil
}

func makeAlias(nic nicInfo) string {
	result := ""
	if nic.L2Type != "" {
		result += fmt.Sprintf("l2type:%s;", nic.L2Type)
	}
	if nic.Category != "" {
		result += fmt.Sprintf("category:%s;", nic.Category)
	}
	if nic.PhysicalInterface != "" {
		result += fmt.Sprintf("physicalInterface:%s;", nic.PhysicalInterface)
	}
	result += fmt.Sprintf("vni:%d;", nic.Vni)
	return result
}

func ConfigureNicEntryPoint()  {
	server.RegisterAsyncCommandHandler(VR_CONFIGURE_NIC, server.VyosLock(configureNic))
	server.RegisterAsyncCommandHandler(VR_REMOVE_NIC_PATH, server.VyosLock(removeNic))
	server.RegisterAsyncCommandHandler(VR_CONFIGURE_NIC_FIREWALL_DEFAULT_ACTION_PATH, server.VyosLock(configureNicFirewallDefaultAction))
}