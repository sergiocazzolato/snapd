// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2017 Canonical Ltd
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

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/apparmor"
	"github.com/snapcore/snapd/interfaces/builtin"
	"github.com/snapcore/snapd/interfaces/seccomp"
	"github.com/snapcore/snapd/interfaces/udev"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/testutil"
)

type OfonoInterfaceSuite struct {
	iface    interfaces.Interface
	slotInfo *snap.SlotInfo
	slot     *interfaces.ConnectedSlot
	plugInfo *snap.PlugInfo
	plug     *interfaces.ConnectedPlug
}

var _ = Suite(&OfonoInterfaceSuite{
	iface: builtin.MustInterface("ofono"),
})

func (s *OfonoInterfaceSuite) SetUpTest(c *C) {
	var mockPlugSnapInfoYaml = `name: other
version: 1.0
apps:
 app:
  command: foo
  plugs: [ofono]
`
	const mockSlotSnapInfoYaml = `name: ofono
version: 1.0
slots:
 ofono:
  interface: ofono
apps:
 app:
  command: foo
  slots: [ofono]
`
	s.slot, s.slotInfo = MockConnectedSlot(c, mockSlotSnapInfoYaml, nil, "ofono")
	s.plug, s.plugInfo = MockConnectedPlug(c, mockPlugSnapInfoYaml, nil, "ofono")
}

func (s *OfonoInterfaceSuite) TestName(c *C) {
	c.Assert(s.iface.Name(), Equals, "ofono")
}

// The label glob when all apps are bound to the ofono slot
func (s *OfonoInterfaceSuite) TestConnectedPlugSnippetUsesSlotLabelAll(c *C) {
	appSet := appSetWithApps(c, "ofono", "app1", "app2")
	si := appSet.Info()
	slot := interfaces.NewConnectedSlot(&snap.SlotInfo{
		Snap:      si,
		Name:      "ofono",
		Interface: "ofono",
		Apps:      map[string]*snap.AppInfo{"app1": si.Apps["app1"], "app2": si.Apps["app2"]},
	}, appSet, nil, nil)

	release.OnClassic = false

	apparmorSpec := apparmor.NewSpecification(s.plug.AppSet())
	err := apparmorSpec.AddConnectedPlug(s.iface, s.plug, slot)
	c.Assert(err, IsNil)
	c.Assert(apparmorSpec.SecurityTags(), DeepEquals, []string{"snap.other.app"})
	c.Assert(apparmorSpec.SnippetForTag("snap.other.app"), testutil.Contains, `peer=(label="snap.ofono.*"),`)
}

// The label uses alternation when some, but not all, apps is bound to the ofono slot
func (s *OfonoInterfaceSuite) TestConnectedPlugSnippetUsesSlotLabelSome(c *C) {
	appSet := appSetWithApps(c, "ofono", "app1", "app2", "app3")
	si := appSet.Info()
	slot := interfaces.NewConnectedSlot(&snap.SlotInfo{
		Snap:      si,
		Name:      "ofono",
		Interface: "ofono",
		Apps:      map[string]*snap.AppInfo{"app1": si.Apps["app1"], "app2": si.Apps["app2"]},
	}, appSet, nil, nil)

	release.OnClassic = false

	apparmorSpec := apparmor.NewSpecification(s.plug.AppSet())
	err := apparmorSpec.AddConnectedPlug(s.iface, s.plug, slot)
	c.Assert(err, IsNil)
	c.Assert(apparmorSpec.SecurityTags(), DeepEquals, []string{"snap.other.app"})
	c.Assert(apparmorSpec.SnippetForTag("snap.other.app"), testutil.Contains, `peer=(label="snap.ofono{.app1,.app2}"),`)
}

// The label uses short form when exactly one app is bound to the ofono slot
func (s *OfonoInterfaceSuite) TestConnectedPlugSnippetUsesSlotLabelOne(c *C) {
	appSet := appSetWithApps(c, "ofono", "app")
	si := appSet.Info()
	slot := interfaces.NewConnectedSlot(&snap.SlotInfo{
		Snap:      si,
		Name:      "ofono",
		Interface: "ofono",
		Apps:      map[string]*snap.AppInfo{"app": si.Apps["app"]},
	}, appSet, nil, nil)

	release.OnClassic = false

	apparmorSpec := apparmor.NewSpecification(s.plug.AppSet())
	err := apparmorSpec.AddConnectedPlug(s.iface, s.plug, slot)
	c.Assert(err, IsNil)
	c.Assert(apparmorSpec.SecurityTags(), DeepEquals, []string{"snap.other.app"})
	c.Assert(apparmorSpec.SnippetForTag("snap.other.app"), testutil.Contains, `peer=(label="snap.ofono.app"),`)
}

func (s *OfonoInterfaceSuite) TestConnectedPlugSnippetUsesUnconfinedLabelOnClassic(c *C) {
	release.OnClassic = true

	apparmorSpec := apparmor.NewSpecification(s.plug.AppSet())
	err := apparmorSpec.AddConnectedPlug(s.iface, s.plug, s.slot)
	c.Assert(err, IsNil)
	c.Assert(apparmorSpec.SecurityTags(), DeepEquals, []string{"snap.other.app"})
	snippet := apparmorSpec.SnippetForTag("snap.other.app")
	// verify apparmor connected
	c.Assert(snippet, testutil.Contains, "#include <abstractions/dbus-strict>")
	// verify classic connected
	c.Assert(snippet, testutil.Contains, "peer=(label=unconfined),")
}

func (s *OfonoInterfaceSuite) TestConnectedPlugSnippetAppArmor(c *C) {
	release.OnClassic = false
	apparmorSpec := apparmor.NewSpecification(s.plug.AppSet())
	err := apparmorSpec.AddConnectedPlug(s.iface, s.plug, s.slot)
	c.Assert(err, IsNil)
	c.Assert(apparmorSpec.SecurityTags(), DeepEquals, []string{"snap.other.app"})
	snippet := apparmorSpec.SnippetForTag("snap.other.app")
	// verify apparmor connected
	c.Assert(snippet, testutil.Contains, "#include <abstractions/dbus-strict>")
	// verify classic didn't connect
	c.Assert(snippet, Not(testutil.Contains), "peer=(label=unconfined),")
}

func (s *OfonoInterfaceSuite) TestConnectedSlotSnippetAppArmor(c *C) {
	apparmorSpec := apparmor.NewSpecification(s.slot.AppSet())
	err := apparmorSpec.AddConnectedSlot(s.iface, s.plug, s.slot)
	c.Assert(err, IsNil)
	aasnippets := apparmorSpec.Snippets()
	c.Assert(aasnippets, HasLen, 1)
	c.Assert(aasnippets["snap.ofono.app"], HasLen, 1)
	snippet := string(aasnippets["snap.ofono.app"][0])
	c.Check(string(snippet), testutil.Contains, "peer=(label=\"snap.other.app\")")
}

func (s *OfonoInterfaceSuite) TestPermanentSlotSnippetAppArmor(c *C) {
	apparmorSpec := apparmor.NewSpecification(s.slot.AppSet())
	err := apparmorSpec.AddPermanentSlot(s.iface, s.slotInfo)
	c.Assert(err, IsNil)
	c.Assert(apparmorSpec.SecurityTags(), DeepEquals, []string{"snap.ofono.app"})
	c.Assert(apparmorSpec.SnippetForTag("snap.ofono.app"), testutil.Contains, "/dev/net/tun rw,")
}

func (s *OfonoInterfaceSuite) TestPermanentSlotSnippetSecComp(c *C) {
	seccompSpec := seccomp.NewSpecification(s.slot.AppSet())
	err := seccompSpec.AddPermanentSlot(s.iface, s.slotInfo)
	c.Assert(err, IsNil)
	c.Assert(seccompSpec.SecurityTags(), DeepEquals, []string{"snap.ofono.app"})
	c.Assert(seccompSpec.SnippetForTag("snap.ofono.app"), testutil.Contains, "listen\n")
}

func (s *OfonoInterfaceSuite) TestPermanentSlotSnippetUDev(c *C) {
	spec := udev.NewSpecification(s.slot.AppSet())
	c.Assert(spec.AddPermanentSlot(s.iface, s.slotInfo), IsNil)
	c.Assert(spec.Snippets(), HasLen, 5)
	c.Assert(spec.Snippets()[0], testutil.Contains, `LABEL="ofono_isi_end"`)
	c.Assert(spec.Snippets(), testutil.Contains, `# ofono
KERNEL=="tty[a-zA-Z]*[0-9]*|cdc-wdm[0-9]*", TAG+="snap_ofono_app"`)
	c.Assert(spec.Snippets(), testutil.Contains, `# ofono
KERNEL=="tun", TAG+="snap_ofono_app"`)
	c.Assert(spec.Snippets(), testutil.Contains, `# ofono
KERNEL=="dsp", TAG+="snap_ofono_app"`)
	c.Assert(spec.Snippets(), testutil.Contains, fmt.Sprintf(`TAG=="snap_ofono_app", SUBSYSTEM!="module", SUBSYSTEM!="subsystem", RUN+="%v/snap-device-helper $env{ACTION} snap_ofono_app $devpath $major:$minor"`, dirs.DistroLibExecDir))
}

func (s *OfonoInterfaceSuite) TestInterfaces(c *C) {
	c.Check(builtin.Interfaces(), testutil.DeepContains, s.iface)
}
