package main

import (
	"context"
	"time"
)

const configVersion = 1

type Config struct {
	Version         int            `json:"version"`
	IntervalSeconds int            `json:"interval_seconds"`
	TTL             int            `json:"ttl"`
	Records         []RecordConfig `json:"records"`
}

type RecordConfig struct {
	Type          string   `json:"type"`
	Name          string   `json:"name"`
	ZoneID        string   `json:"zone_id"`
	ZoneName      string   `json:"zone_name"`
	RecordID      string   `json:"record_id,omitempty"`
	AllowMultiple bool     `json:"allow_multiple,omitempty"`
	Enabled       bool     `json:"enabled"`
	Source        IPSource `json:"source"`
}

type IPSource struct {
	Mode      string `json:"mode"`
	Interface string `json:"interface,omitempty"`
	URL       string `json:"url,omitempty"`
}

type State struct {
	Version int                     `json:"version"`
	Records map[string]*RecordState `json:"records"`
}

type RecordState struct {
	Name           string    `json:"name"`
	Address        string    `json:"address,omitempty"`
	LastCheck      time.Time `json:"last_check,omitempty"`
	LastSuccess    time.Time `json:"last_success,omitempty"`
	LastChange     time.Time `json:"last_change,omitempty"`
	LastCloudCheck time.Time `json:"last_cloud_check,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
}

type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type DNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type CloudAPI interface {
	VerifyToken(context.Context) error
	ListZones(context.Context) ([]Zone, error)
	FindRecords(context.Context, string, string) ([]DNSRecord, error)
	GetRecord(context.Context, string, string) (DNSRecord, error)
	CreateRecord(context.Context, string, DNSRecord) (DNSRecord, error)
	PatchRecord(context.Context, string, string, map[string]any) (DNSRecord, error)
}

type AddressResolver interface {
	Resolve(context.Context, IPSource, string, string) (string, error)
}
