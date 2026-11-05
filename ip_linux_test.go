//go:build linux

package main

import (
	"encoding/binary"
	"net/netip"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxIPv6Eligibility(t *testing.T) {
	if !eligibleIPv6(unix.IFA_F_PERMANENT, 3600, 1800) || !eligibleIPv6(unix.IFA_F_TEMPORARY, ^uint32(0), ^uint32(0)) {
		t.Fatal("valid preferred IPv6 rejected")
	}
	for _, flag := range []uint32{unix.IFA_F_TENTATIVE, unix.IFA_F_DADFAILED, unix.IFA_F_DEPRECATED} {
		if eligibleIPv6(flag, 3600, 1800) {
			t.Errorf("unusable flags accepted: %x", flag)
		}
	}
	if eligibleIPv6(0, 0, 1800) || eligibleIPv6(0, 3600, 0) {
		t.Fatal("expired or deprecated lifetime accepted")
	}
}

func TestLinuxAddressDumpFiltering(t *testing.T) {
	attribute := func(kind uint16, value []byte) []byte {
		length := 4 + len(value)
		data := make([]byte, (length+3)&^3)
		binary.NativeEndian.PutUint16(data[:2], uint16(length))
		binary.NativeEndian.PutUint16(data[2:4], kind)
		copy(data[4:], value)
		return data
	}
	message := func(index int, address string, flags, valid, preferred uint32) syscall.NetlinkMessage {
		data := make([]byte, unix.SizeofIfAddrmsg)
		data[0] = unix.AF_INET6
		binary.NativeEndian.PutUint32(data[4:8], uint32(index))
		rawAddress := netip.MustParseAddr(address).As16()
		data = append(data, attribute(unix.IFA_ADDRESS, rawAddress[:])...)
		flagBytes := make([]byte, 4)
		binary.NativeEndian.PutUint32(flagBytes, flags)
		data = append(data, attribute(unix.IFA_FLAGS, flagBytes)...)
		cacheInfo := make([]byte, 16)
		binary.NativeEndian.PutUint32(cacheInfo[:4], preferred)
		binary.NativeEndian.PutUint32(cacheInfo[4:8], valid)
		data = append(data, attribute(unix.IFA_CACHEINFO, cacheInfo)...)
		return syscall.NetlinkMessage{Header: syscall.NlMsghdr{Type: syscall.RTM_NEWADDR}, Data: data}
	}
	messages := []syscall.NetlinkMessage{
		message(3, "2606:4700::1", unix.IFA_F_TEMPORARY, 3600, 1800),
		message(3, "2606:4700::2", unix.IFA_F_PERMANENT, 3600, 1800),
		message(3, "2606:4700::3", unix.IFA_F_TENTATIVE, 3600, 1800),
		message(3, "2606:4700::4", unix.IFA_F_DEPRECATED, 3600, 1800),
		message(3, "2606:4700::5", unix.IFA_F_DADFAILED, 3600, 1800),
		message(3, "2606:4700::6", 0, 3600, 0),
		message(3, "2606:4700::7", 0, 0, 1800),
		message(3, "fe80::1", 0, 3600, 1800),
		message(4, "2606:4700::8", 0, 3600, 1800),
	}
	addresses, err := parseIPv6Candidates(messages, 3)
	if err != nil || len(addresses) != 2 {
		t.Fatalf("dump filtering = %v, %v", addresses, err)
	}
	if !addresses[0].Temporary || addresses[1].Temporary {
		t.Fatalf("temporary flags lost: %v", addresses)
	}
	chosen, err := selectAddress(addresses, "AAAA", "")
	if err != nil || chosen != "2606:4700::2" {
		t.Fatalf("dump selection = %s, %v", chosen, err)
	}
}
