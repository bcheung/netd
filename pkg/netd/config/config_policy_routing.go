/*
Copyright 2018 Google Inc.

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

package config

import (
	"errors"
	"fmt"
	"net"

	"github.com/GoogleCloudPlatform/netd/internal/systemutil"

	"github.com/containernetworking/plugins/pkg/utils/sysctl"
	"github.com/golang/glog"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/netd/internal/ipt"
)

const (
	sysctlSrcValidMark = "net.ipv4.conf.all.src_valid_mark"
)

const (
	tableMangle         = "mangle"
	preRoutingChain     = "PREROUTING"
	postRoutingChain    = "POSTROUTING"
	gcpPreRoutingChain  = "GCP-PREROUTING"
	gcpPostRoutingChain = "GCP-POSTROUTING"
	hairpinMark         = 0x4000
	hairpinMask         = 0x4000
)
const (
	policyRoutingGcpPreRoutingComment  = "restore the conn mark if applicable"
	policyRoutingPreRoutingComment     = "redirect all traffic to GCP-PREROUTING chain"
	policyRoutingGcpPostRoutingComment = "save the conn mark only if hairpin bit (0x4000/0x4000) is set"
	policyRoutingPostRoutingComment    = "redirect all traffic to GCP-POSTROUTING chain"
)

const (
	customRouteTable    = 1
	hairpinRulePriority = 30000 + iota
	localRulePriority
	policyRoutingRulePriority
)

var (
	defaultGateway   net.IP
	defaultLinkIndex int
	defaultNetdev    string
	localLinkIndex   int
	localNetdev      string
)

// PolicyRoutingConfigSet defines the Policy Routing rules
var PolicyRoutingConfigSet = Set{
	false,
	"PolicyRouting",
	nil,
}

func init() {
	f := func(ip net.IP) (linkIndex int, netdev string, gw net.IP) {
		nic, err := systemutil.GetNIC(ip)
		if err != nil {
			glog.Error(err)
			if errors.Is(err, systemutil.ErrFailedRoute) {
				return
			}
		}

		gw = nic.Route.Gw
		linkIndex = nic.Route.LinkIndex
		netdev = nic.Link.Name

		return
	}
	defaultLinkIndex, defaultNetdev, defaultGateway = f(net.IPv4(8, 8, 8, 8))
	localLinkIndex, localNetdev, _ = f(net.IPv4(127, 0, 0, 1))

	sysctlReversePathFilter := fmt.Sprintf("net.ipv4.conf.%s.rp_filter", defaultNetdev)
	PolicyRoutingConfigSet.Configs = []Config{
		SysctlConfig{
			Key:          sysctlReversePathFilter,
			Value:        "2",
			DefaultValue: "1",
			SysctlFunc:   sysctl.Sysctl,
		},
		SysctlConfig{
			Key:          sysctlSrcValidMark,
			Value:        "1",
			DefaultValue: "0",
			SysctlFunc:   sysctl.Sysctl,
		},
		IPTablesRulesConfig{
			Spec: ipt.IPTablesSpec{
				TableName: tableMangle,
				ChainName: gcpPreRoutingChain,
				Rules: []ipt.IPTablesRule{
					[]string{"-j", "CONNMARK", "--restore-mark", "-m", "comment", "--comment", policyRoutingGcpPreRoutingComment},
				},
				IPT: ipt.IPv4Tables,
			},
			IsDefaultChain: false,
		},
		IPTablesRulesConfig{
			Spec: ipt.IPTablesSpec{
				TableName: tableMangle,
				ChainName: preRoutingChain,
				Rules: []ipt.IPTablesRule{
					[]string{"-j", gcpPreRoutingChain, "-m", "comment", "--comment", policyRoutingPreRoutingComment},
				},
				IPT: ipt.IPv4Tables,
			},
			IsDefaultChain: true,
		},
		IPTablesRulesConfig{
			Spec: ipt.IPTablesSpec{
				TableName: tableMangle,
				ChainName: gcpPostRoutingChain,
				Rules: []ipt.IPTablesRule{
					[]string{"-m", "mark", "--mark",
						fmt.Sprintf("0x%x/0x%x", hairpinMark, hairpinMask),
						"-j", "CONNMARK", "--save-mark", "-m", "comment", "--comment", policyRoutingGcpPostRoutingComment},
				},
				IPT: ipt.IPv4Tables,
			},
			IsDefaultChain: false,
		},
		IPTablesRulesConfig{
			Spec: ipt.IPTablesSpec{
				TableName: tableMangle,
				ChainName: postRoutingChain,
				Rules: []ipt.IPTablesRule{
					[]string{"-j", gcpPostRoutingChain, "-m", "comment", "--comment", policyRoutingPostRoutingComment},
				},
				IPT: ipt.IPv4Tables,
			},
			IsDefaultChain: true,
		},
		IPRouteConfig{
			Route: netlink.Route{
				Table:     customRouteTable,
				LinkIndex: defaultLinkIndex,
				Gw:        defaultGateway,
				Dst:       nil,
			},
			RouteAdd: netlink.RouteAdd,
			RouteDel: netlink.RouteDel,
		},
		IPRuleConfig{
			Rule: netlink.Rule{
				Mark:              hairpinMark,
				Mask:              hairpinMask,
				Table:             unix.RT_TABLE_MAIN,
				Priority:          hairpinRulePriority,
				SuppressIfgroup:   -1,
				SuppressPrefixlen: -1,
				Goto:              -1,
				Flow:              -1,
			},
			RuleAdd:  netlink.RuleAdd,
			RuleDel:  netlink.RuleDel,
			RuleList: netlink.RuleList,
		},
		IPRuleConfig{
			Rule: netlink.Rule{
				IifName:           localNetdev,
				Table:             unix.RT_TABLE_MAIN,
				Priority:          localRulePriority,
				SuppressIfgroup:   -1,
				SuppressPrefixlen: -1,
				Mark:              -1,
				Mask:              -1,
				Goto:              -1,
				Flow:              -1,
			},
			RuleAdd:  netlink.RuleAdd,
			RuleDel:  netlink.RuleDel,
			RuleList: netlink.RuleList,
		},
		IPRuleConfig{
			Rule: netlink.Rule{
				IifName:           defaultNetdev,
				Invert:            true,
				Table:             customRouteTable,
				Priority:          policyRoutingRulePriority,
				SuppressIfgroup:   -1,
				SuppressPrefixlen: -1,
				Mark:              -1,
				Mask:              -1,
				Goto:              -1,
				Flow:              -1,
			},
			RuleAdd:  netlink.RuleAdd,
			RuleDel:  netlink.RuleDel,
			RuleList: netlink.RuleList,
		},
	}
}