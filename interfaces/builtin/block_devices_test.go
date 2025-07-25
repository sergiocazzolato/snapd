// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package builtin_test

import (
	"fmt"
	"strings"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/apparmor"
	"github.com/snapcore/snapd/interfaces/builtin"
	"github.com/snapcore/snapd/interfaces/udev"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/testutil"
)

type blockDevicesInterfaceSuite struct {
	testutil.BaseTest

	iface    interfaces.Interface
	slotInfo *snap.SlotInfo
	slot     *interfaces.ConnectedSlot
	plugInfo *snap.PlugInfo
	plug     *interfaces.ConnectedPlug
}

var _ = Suite(&blockDevicesInterfaceSuite{
	iface: builtin.MustInterface("block-devices"),
})

const blockDevicesConsumerYaml = `name: consumer
version: 0
apps:
 app:
  plugs: [block-devices]
`

const blockDevicesWithPartitionsConsumerYaml = `name: consumer
version: 0
apps:
 app:
  plugs: [block-devices]
plugs:
 block-devices:
  allow-partitions: true
`

const blockDevicesCoreYaml = `name: core
version: 0
type: os
slots:
  block-devices:
`

func (s *blockDevicesInterfaceSuite) SetUpTest(c *C) {
	s.BaseTest.SetUpTest(c)

	s.plug, s.plugInfo = MockConnectedPlug(c, blockDevicesConsumerYaml, nil, "block-devices")
	s.slot, s.slotInfo = MockConnectedSlot(c, blockDevicesCoreYaml, nil, "block-devices")
}

func (s *blockDevicesInterfaceSuite) TestName(c *C) {
	c.Assert(s.iface.Name(), Equals, "block-devices")
}

func (s *blockDevicesInterfaceSuite) TestSanitizeSlot(c *C) {
	c.Assert(interfaces.BeforePrepareSlot(s.iface, s.slotInfo), IsNil)
}

func (s *blockDevicesInterfaceSuite) TestSanitizePlug(c *C) {
	c.Assert(interfaces.BeforePreparePlug(s.iface, s.plugInfo), IsNil)
}

func (s *blockDevicesInterfaceSuite) TestSanitizePlugWithPartitions(c *C) {
	_, s.plugInfo = MockConnectedPlug(c, blockDevicesWithPartitionsConsumerYaml, nil, "block-devices")
	c.Assert(interfaces.BeforePreparePlug(s.iface, s.plugInfo), IsNil)
}

func (s *blockDevicesInterfaceSuite) TestSanitizePlugWithInvalidPartitions(c *C) {
	const badPartitions = `name: consumer
version: 0
apps:
 app:
  plugs: [block-devices]
plugs:
 block-devices:
  allow-partitions: yes-please
`
	_, s.plugInfo = MockConnectedPlug(c, badPartitions, nil, "block-devices")
	c.Assert(interfaces.BeforePreparePlug(s.iface, s.plugInfo), ErrorMatches,
		`block-devices "allow-partitions" attribute must be boolean`)
}

func (s *blockDevicesInterfaceSuite) TestAppArmorSpec(c *C) {
	appSet, err := interfaces.NewSnapAppSet(s.plug.Snap(), nil)
	c.Assert(err, IsNil)
	spec := apparmor.NewSpecification(appSet)
	c.Assert(spec.AddConnectedPlug(s.iface, s.plug, s.slot), IsNil)
	c.Assert(spec.SecurityTags(), DeepEquals, []string{"snap.consumer.app"})
	c.Assert(spec.SnippetForTag("snap.consumer.app"), testutil.Contains, `# Description: Allow write access to raw disk block devices.`)
	c.Assert(spec.SnippetForTag("snap.consumer.app"), testutil.Contains, `/dev/sd{,[a-h]}[a-z] rwk,`)
}

func (s *blockDevicesInterfaceSuite) TestAppArmorSpecWithPartitions(c *C) {
	s.plug, s.plugInfo = MockConnectedPlug(c, blockDevicesWithPartitionsConsumerYaml, nil, "block-devices")
	appSet, err := interfaces.NewSnapAppSet(s.plug.Snap(), nil)
	c.Assert(err, IsNil)
	spec := apparmor.NewSpecification(appSet)
	c.Assert(spec.AddConnectedPlug(s.iface, s.plug, s.slot), IsNil)
	c.Assert(spec.SecurityTags(), DeepEquals, []string{"snap.consumer.app"})
	c.Assert(spec.SnippetForTag("snap.consumer.app"), testutil.Contains, `# Description: Allow write access to raw disk block devices.`)
	c.Assert(spec.SnippetForTag("snap.consumer.app"), testutil.Contains, `/dev/sd[a-z][1-9]{,[0-6]} rwk,`)
}

func (s *blockDevicesInterfaceSuite) TestUDevSpec(c *C) {
	appSet, err := interfaces.NewSnapAppSet(s.plug.Snap(), nil)
	c.Assert(err, IsNil)
	spec := udev.NewSpecification(appSet)
	c.Assert(spec.AddConnectedPlug(s.iface, s.plug, s.slot), IsNil)
	c.Assert(spec.Snippets(), HasLen, 6)
	c.Assert(spec.Snippets()[0], Equals, `# block-devices
KERNEL=="megaraid_sas_ioctl_node", TAG+="snap_consumer_app"`)
	c.Assert(spec.Snippets(), testutil.Contains,
		fmt.Sprintf(`TAG=="snap_consumer_app", SUBSYSTEM!="module", SUBSYSTEM!="subsystem", RUN+="%v/snap-device-helper $env{ACTION} snap_consumer_app $devpath $major:$minor"`, dirs.DistroLibExecDir))
	all := strings.Join(spec.Snippets(), "\n")
	c.Logf("all snippets:\n%s", all)
	c.Assert(all, Not(testutil.Contains), "partition")
}

func (s *blockDevicesInterfaceSuite) TestUDevSpecWitPartitions(c *C) {
	s.plug, s.plugInfo = MockConnectedPlug(c, blockDevicesWithPartitionsConsumerYaml, nil, "block-devices")
	appSet, err := interfaces.NewSnapAppSet(s.plug.Snap(), nil)
	c.Assert(err, IsNil)
	spec := udev.NewSpecification(appSet)
	c.Assert(spec.AddConnectedPlug(s.iface, s.plug, s.slot), IsNil)
	c.Assert(spec.Snippets(), HasLen, 7)
	all := strings.Join(spec.Snippets(), "\n")
	c.Logf("all snippets:\n%s", all)
	c.Assert(all, testutil.Contains,
		`SUBSYSTEM=="block", ENV{DEVTYPE}=="partition", TAG+="snap_consumer_app"`)
	c.Assert(spec.Snippets(), testutil.Contains,
		fmt.Sprintf(`TAG=="snap_consumer_app", SUBSYSTEM!="module", SUBSYSTEM!="subsystem", RUN+="%v/snap-device-helper $env{ACTION} snap_consumer_app $devpath $major:$minor"`, dirs.DistroLibExecDir))
}

func (s *blockDevicesInterfaceSuite) TestStaticInfo(c *C) {
	si := interfaces.StaticInfoOf(s.iface)
	c.Assert(si.ImplicitOnCore, Equals, true)
	c.Assert(si.ImplicitOnClassic, Equals, true)
	c.Assert(si.Summary, Equals, `allows access to disk block devices`)
	c.Assert(si.BaseDeclarationSlots, testutil.Contains, "block-devices")
}

func (s *blockDevicesInterfaceSuite) TestAutoConnect(c *C) {
	c.Assert(s.iface.AutoConnect(s.plugInfo, s.slotInfo), Equals, true)
}

func (s *blockDevicesInterfaceSuite) TestInterfaces(c *C) {
	c.Check(builtin.Interfaces(), testutil.DeepContains, s.iface)
}
