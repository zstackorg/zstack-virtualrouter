package plugin

import (
	"zvr/server"
	"fmt"
	"zvr/utils"
	log "github.com/Sirupsen/logrus"
	"strings"
	"time"
)

const (
	SET_VYOSHA_PATH = "/enableVyosha"
)

type setVyosHaCmd struct {
	Keepalive int  `json:"keepalive"`
	HeartbeatNic string `json:"heartbeatNic"`
	LocalIp string `json:"localIp"`
	PeerIp string `json:"peerIp"`
	Monitors []string `json:"monitors"`
	Vips []macVipPair `json:"vips"`
}

type macVipPair struct {
	NicMac string     	`json:"nicMac"`
	NicVip     string  	`json:"nicVip"`
	Netmask     string  	`json:"netmask"`
	Category     string  	`json:"category"`
}

var vyosIsMaster bool

func setVyosHaHandler(ctx *server.CommandContext) interface{} {
	cmd := &setVyosHaCmd{}
	ctx.GetCommand(cmd)

	heartbeatNicNme, _ := utils.GetNicNameByMac(cmd.HeartbeatNic)
	/* add firewall */
	tree := server.NewParserFromShowConfiguration().Tree
	if utils.IsSkipVyosIptables() {
		/* TODO */
	} else {
		des := "Vyos-HA"
		if fr := tree.FindFirewallRuleByDescription(heartbeatNicNme, "local", des); fr == nil {
			tree.SetFirewallOnInterface(heartbeatNicNme, "local",
				"action accept",
				fmt.Sprintf("description %v", des),
				fmt.Sprintf("source address %v", cmd.PeerIp),
				fmt.Sprintf("protocol vrrp"),
			)
		}
	}

	pairs := []nicVipPair{}
	for _, p := range cmd.Vips {
		nicname, err := utils.GetNicNameByMac(p.NicMac); utils.PanicOnError(err)
		cidr, err := utils.NetmaskToCIDR(p.Netmask); utils.PanicOnError(err)
		pairs = append(pairs, nicVipPair{NicName: nicname, Vip: p.NicVip, Prefix:cidr})

		addSecondaryIpFirewall(nicname, p.NicVip, tree)

		tree.AttachFirewallToInterface(nicname, "local")
	}

	tree.Apply(false)

	/* generate notify script first */
	addHaNicVipPair(pairs)

	if cmd.PeerIp == "" {
		cmd.PeerIp = cmd.LocalIp
	}
	checksum, err := getFileChecksum(KeepalivedConfigFile);utils.PanicOnError(err)
	keepalivedConf := NewKeepalivedConf(heartbeatNicNme, cmd.LocalIp, cmd.PeerIp, cmd.Monitors, cmd.Keepalive)
	keepalivedConf.BuildConf()
	newCheckSum, err := getFileChecksum(KeepalivedConfigFile);utils.PanicOnError(err)
	if newCheckSum != checksum {
		keepalivedConf.RestartKeepalived()
	} else {
		log.Debugf("keepalived configure file unchanged")
	}

	return nil
}

func IsMaster() bool {
	if !utils.IsHaEabled() {
		return true
	}

	return vyosIsMaster
}

func getHaStatus() bool {
	bash := utils.Bash{
		Command: fmt.Sprintf("cat %s", KeepalivedStateFile),
		NoLog: true,
	}

	ret, o, _, err := bash.RunWithReturn()
	if err != nil || ret != 0 {
		return vyosIsMaster
	}

	if strings.Contains(o, "MASTER") {
		return true
	} else {
		return false
	}
}

func vyosHaStatusCheckTask()  {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for  {
		select {
		case <-ticker.C:
		        if utils.IsHaEabled() {
				newHaStatus := getHaStatus()
				if newHaStatus == vyosIsMaster {
					continue
				}

				/* there is a situation when zvr write the keepalived notify script,
		           	at the same time keepalived is changing state,
		           	so when zvr detect status change, all script again to make sure no missing config */
				vyosIsMaster = newHaStatus
				server.VyosLockInterface(callStatusChangeScripts)()
			}
		}
	}
}

func keepAlivedCheckTask()  {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for  {
		select {
		case <-ticker.C:
		        if utils.IsHaEabled() {
				checkKeepalivedRunning()
			}
		}
	}
}

type nicVipPair struct {
	NicName string
	Vip     string
	Prefix  int
}

type vyosNicVipPairs struct {
	pairs []nicVipPair
}

func generateNotigyScripts(vyosHaVips []nicVipPair)  {
	keepalivedNofityConf := NewKeepalivedNotifyConf(vyosHaVips)
	keepalivedNofityConf.CreateMasterScript()
	keepalivedNofityConf.CreateBackupScript()
}

func addHaNicVipPair(pairs []nicVipPair)  {
	count := 0
	for _, p := range pairs {
		found := false
		for _, op := range haVipPairs.pairs {
			if p.NicName == op.NicName && p.Vip == op.Vip {
				found = true
				break
			}
		}

		if !found {
			count ++;
			haVipPairs.pairs = append(haVipPairs.pairs, p)
		}
	}

	generateNotigyScripts(haVipPairs.pairs)
}

func removeHaNicVipPair(pairs []nicVipPair)  {
	newPair := []nicVipPair{}
	for _, p := range haVipPairs.pairs {
		found := false
		for _, np := range pairs {
			if p.NicName == np.NicName && p.Vip == np.Vip {
				found = true
				break
			}
		}

		if !found {
			newPair = append(newPair, p)
		}
	}

	if len(newPair) != len(haVipPairs.pairs) {
		haVipPairs.pairs = newPair
		generateNotigyScripts(haVipPairs.pairs)
	}
}

func InitHaNicState()  {
	if !utils.IsHaEabled() {
		return
	}

	/* if ha is enable, shutdown all interface except eth0 */
	cmds := []string{}
	nics, _ := utils.GetAllNics()
	for _, nic := range nics {
		if nic.Name == "eth0" {
			continue
		}

		if strings.Contains(nic.Name, "eth") {
			cmds = append(cmds, fmt.Sprintf("ip link set dev %v down", nic.Name))
		}
	}

	cmds = append(cmds, fmt.Sprintf("sudo sysctl -w net.ipv4.ip_nonlocal_bind=1"))
	b := utils.Bash{
		Command: strings.Join(cmds, "\n"),
	}

	b.Run()
	b.PanicIfError()

	callStatusChangeScripts()
}


var haVipPairs  vyosNicVipPairs
func init() {
	vyosIsMaster = false
	haVipPairs.pairs = []nicVipPair{}
}

func VyosHaEntryPoint() {
	server.RegisterAsyncCommandHandler(SET_VYOSHA_PATH, server.VyosLock(setVyosHaHandler))
	if utils.IsHaEabled() {
		go vyosHaStatusCheckTask()
		go keepAlivedCheckTask()
	}
}