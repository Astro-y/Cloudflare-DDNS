//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

func interfaceCandidates(name, recordType string) ([]addressCandidate, error) {
	if recordType == "A" {
		return genericInterfaceCandidates(name, recordType)
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("网卡 %s 不存在", name)
	}
	if iface.Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("网卡 %s 未启用", name)
	}
	data, err := syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_INET6)
	if err != nil {
		return nil, fmt.Errorf("无法读取 IPv6 地址状态：%w", err)
	}
	messages, err := syscall.ParseNetlinkMessage(data)
	if err != nil {
		return nil, errors.New("无法解析 IPv6 地址状态")
	}
	return parseIPv6Candidates(messages, iface.Index)
}

func parseIPv6Candidates(messages []syscall.NetlinkMessage, interfaceIndex int) ([]addressCandidate, error) {
	var candidates []addressCandidate
	for _, msg := range messages {
		if msg.Header.Type != syscall.RTM_NEWADDR || len(msg.Data) < unix.SizeofIfAddrmsg {
			continue
		}
		if msg.Data[0] != syscall.AF_INET6 || binary.NativeEndian.Uint32(msg.Data[4:8]) != uint32(interfaceIndex) {
			continue
		}
		attributes, err := syscall.ParseNetlinkRouteAttr(&msg)
		if err != nil {
			return nil, errors.New("无法解析 IPv6 地址属性")
		}
		var rawAddress []byte
		flags := uint32(msg.Data[2])
		valid, preferred := uint32(1), uint32(1)
		for _, attr := range attributes {
			switch attr.Attr.Type {
			case unix.IFA_ADDRESS:
				rawAddress = attr.Value
			case unix.IFA_FLAGS:
				if len(attr.Value) >= 4 {
					flags = binary.NativeEndian.Uint32(attr.Value)
				}
			case unix.IFA_CACHEINFO:
				if len(attr.Value) >= 8 {
					preferred = binary.NativeEndian.Uint32(attr.Value[:4])
					valid = binary.NativeEndian.Uint32(attr.Value[4:8])
				}
			}
		}
		address, ok := netip.AddrFromSlice(rawAddress)
		if !ok || !eligibleIPv6(flags, valid, preferred) || !isPublicAddr(address) {
			continue
		}
		candidates = append(candidates, addressCandidate{Address: address, Temporary: flags&unix.IFA_F_TEMPORARY != 0})
	}
	return candidates, nil
}

func eligibleIPv6(flags, valid, preferred uint32) bool {
	return flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED|unix.IFA_F_DEPRECATED) == 0 && valid != 0 && preferred != 0
}
