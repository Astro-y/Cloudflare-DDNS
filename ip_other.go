//go:build !linux

package main

func interfaceCandidates(name, recordType string) ([]addressCandidate, error) {
	return genericInterfaceCandidates(name, recordType)
}
