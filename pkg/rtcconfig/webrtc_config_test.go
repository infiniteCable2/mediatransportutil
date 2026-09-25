// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rtcconfig

import (
	"net"
	"sync/atomic"
	"testing"

	"github.com/pion/stun/v3"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

func Test_IPFilterFromConf(t *testing.T) {
	testData := IPsConfig{
		Includes: []string{"10.0.0.0/19"},
		Excludes: []string{"10.0.0.0/9", "10.192.0.0/11", "10.244.0.0/16", "172.16.0.0/12", "192.168.128.0/17"},
	}

	ipFilter, err := IPFilterFromConf(testData)
	require.NoError(t, err)

	testCases := []struct {
		ip       string
		expected bool
	}{
		{"10.0.0.10", true},
		{"10.0.0.1", true},
		{"10.0.31.255", true},
		{"10.0.32.1", false},
		{"10.192.0.1", false},
		{"10.244.0.1", false},
		{"172.16.0.10", false},
		{"192.168.128.5", false},
	}

	for _, tc := range testCases {
		testIP := net.ParseIP(tc.ip)

		if result := ipFilter(testIP); result != tc.expected {
			t.Errorf("For IP %s, expected %v but got %v", tc.ip, tc.expected, result)
		}
	}

	testData = IPsConfig{
		Includes: []string{"192.168.128.1"},
		Excludes: []string{"192.168.128.0/17"},
	}
	_, err = IPFilterFromConf(testData)
	require.Error(t, err)
}

func Test_InterfaceFilterFromConf(t *testing.T) {
	testData := InterfacesConfig{
		Includes: []string{"eth0", "eth1", "eth2"},
		Excludes: []string{"eth0", "eth3", "eth4"},
	}

	ifaceFilter := InterfaceFilterFromConf(testData)

	testCases := []struct {
		iface    string
		expected bool
	}{
		{"eth0", true},
		{"eth1", true},
		{"eth2", true},
		{"eth3", false},
		{"eth4", false},
		{"eth5", false},
	}

	for _, tc := range testCases {
		if result := ifaceFilter(tc.iface); result != tc.expected {
			t.Errorf("For interface %s, expected %v but got %v", tc.iface, tc.expected, result)
		}
	}
}

func Test_NAT1To1AddressRewriteRules(t *testing.T) {
	t.Run("catchAll replaces host candidate when includeInternal is false", func(t *testing.T) {
		rules := nat1To1AddressRewriteRules([]string{"1.2.3.4"}, false)
		require.Len(t, rules, 1)
		require.Equal(t, []string{"1.2.3.4"}, rules[0].External)
		require.Equal(t, webrtc.ICECandidateTypeHost, rules[0].AsCandidateType)
		require.Equal(t, webrtc.ICEAddressRewriteModeUnspecified, rules[0].Mode)
	})

	t.Run("catchAll appends host candidate when includeInternal is true", func(t *testing.T) {
		rules := nat1To1AddressRewriteRules([]string{"1.2.3.4"}, true)
		require.Len(t, rules, 1)
		require.Equal(t, []string{"1.2.3.4"}, rules[0].External)
		require.Equal(t, webrtc.ICECandidateTypeHost, rules[0].AsCandidateType)
		require.Equal(t, webrtc.ICEAddressRewriteAppend, rules[0].Mode)
	})

	t.Run("external/local pairs and catchAll IPs get the same mode", func(t *testing.T) {
		rules := nat1To1AddressRewriteRules([]string{"5.6.7.8/10.0.0.1", "1.2.3.4"}, true)
		require.Len(t, rules, 2)

		require.Equal(t, []string{"5.6.7.8"}, rules[0].External)
		require.Equal(t, "10.0.0.1", rules[0].Local)
		require.Equal(t, webrtc.ICEAddressRewriteAppend, rules[0].Mode)

		require.Equal(t, []string{"1.2.3.4"}, rules[1].External)
		require.Equal(t, webrtc.ICEAddressRewriteAppend, rules[1].Mode)
	})
}

func TestWithExternalIPMappingsKeepsOriginalAndSockets(t *testing.T) {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer listener.Close()

	current := &WebRTCConfig{TCPMuxListener: listener, NAT1To1IPs: []string{"198.51.100.10/192.0.2.10"}}
	conf := &RTCConfig{AdvertiseInternalIP: true}
	newIPs := []string{"198.51.100.20/192.0.2.10"}
	next, err := current.withExternalIPMappings(conf, newIPs, nil)
	require.NoError(t, err)
	require.Same(t, listener, next.TCPMuxListener)
	require.Equal(t, []string{"198.51.100.10/192.0.2.10"}, current.NAT1To1IPs)
	require.Equal(t, newIPs, next.NAT1To1IPs)

	newIPs[0] = "198.51.100.30/192.0.2.10"
	require.Equal(t, []string{"198.51.100.20/192.0.2.10"}, next.NAT1To1IPs)
}

func TestRefreshExternalIPFromLocalSTUNServer(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer server.Close()

	var advertised atomic.Value
	advertised.Store("198.51.100.10")
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := server.ReadFromUDP(buf)
			if err != nil {
				return
			}
			request := stun.New()
			request.Raw = append(request.Raw[:0], buf[:n]...)
			if err := request.Decode(); err != nil {
				continue
			}
			response, err := stun.Build(
				stun.BindingSuccess,
				stun.NewTransactionIDSetter(request.TransactionID),
				&stun.XORMappedAddress{IP: net.ParseIP(advertised.Load().(string)), Port: addr.Port},
			)
			if err == nil {
				_, _ = server.WriteToUDP(response.Raw, addr)
			}
		}
	}()

	conf := &RTCConfig{
		UseExternalIP:           true,
		EnableLoopbackCandidate: true,
		STUNServers:             []string{server.LocalAddr().String()},
		IPs:                     IPsConfig{Includes: []string{"127.0.0.0/8"}},
		NodeIP:                  NodeIP{V4: "198.51.100.10"},
	}
	initial := &WebRTCConfig{NAT1To1IPs: []string{"198.51.100.10/127.0.0.1"}}
	advertised.Store("198.51.100.20")
	first, firstNode, err := initial.RefreshExternalIP(conf)
	require.NoError(t, err)
	require.Equal(t, "198.51.100.20", firstNode.V4)
	require.Equal(t, []string{"198.51.100.20/127.0.0.1"}, first.NAT1To1IPs)
	require.Equal(t, "198.51.100.10", conf.NodeIP.V4)
	require.Equal(t, []string{"198.51.100.10/127.0.0.1"}, initial.NAT1To1IPs)

	advertised.Store("198.51.100.30")
	second, secondNode, err := first.RefreshExternalIP(conf)
	require.NoError(t, err)
	require.Equal(t, "198.51.100.30", secondNode.V4)
	require.Equal(t, []string{"198.51.100.30/127.0.0.1"}, second.NAT1To1IPs)
	require.Equal(t, []string{"198.51.100.20/127.0.0.1"}, first.NAT1To1IPs)
}
