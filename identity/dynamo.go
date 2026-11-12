package main

// Read-only view of the go-sync DynamoDB table so the account page can show
// WHAT is synced (per-type item counts). We never write here — wiping a chain
// must go through go-sync so its cache/consistency stay intact. dynamodb-local
// runs with -sharedDb, so any credentials see the same data; we sign requests
// with SigV4 (stdlib only — no AWS SDK) using the same dummy creds as the
// stack. Counts are best-effort: any failure just hides the section.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"
)

type dynamoClient struct {
	endpoint  string
	host      string
	region    string
	accessKey string
	secretKey string
	table     string
	http      *http.Client
}

// newDynamoFromEnv builds a client from AWS_ENDPOINT/TABLE_NAME, or returns nil
// when they are unset (the counts section is then simply omitted).
func newDynamoFromEnv() *dynamoClient {
	endpoint := os.Getenv("AWS_ENDPOINT")
	table := os.Getenv("TABLE_NAME")
	if endpoint == "" || table == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil
	}
	return &dynamoClient{
		endpoint:  endpoint,
		host:      u.Host,
		region:    envOr("AWS_REGION", "us-west-2"),
		accessKey: envOr("AWS_ACCESS_KEY_ID", "GOSYNC"),
		secretKey: envOr("AWS_SECRET_ACCESS_KEY", "GOSYNC"),
		table:     table,
		http:      &http.Client{Timeout: 8 * time.Second},
	}
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// call signs and sends one DynamoDB JSON request (SigV4).
func (d *dynamoClient) call(target string, payload []byte) ([]byte, error) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	payloadHash := sha256Hex(payload)
	canonicalHeaders := "content-type:application/x-amz-json-1.0\n" +
		"host:" + d.host + "\n" +
		"x-amz-date:" + amzDate + "\n" +
		"x-amz-target:" + target + "\n"
	signedHeaders := "content-type;host;x-amz-date;x-amz-target"
	canonicalRequest := "POST\n/\n\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + payloadHash
	scope := dateStamp + "/" + d.region + "/dynamodb/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))

	kDate := hmacSHA256([]byte("AWS4"+d.secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, d.region)
	kService := hmacSHA256(kRegion, "dynamodb")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))
	authz := "AWS4-HMAC-SHA256 Credential=" + d.accessKey + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature

	req, err := http.NewRequest("POST", d.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Host = d.host
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", target)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Authorization", authz)

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dynamodb %s: %s", resp.Status, string(body))
	}
	return body, nil
}

// TypeCount is one row of the "what's synced" section.
type TypeCount struct {
	Name  string
	Count int
}

// SyncSummary queries all of the account's live entities (projected to just
// DataType + Deleted) and returns friendly per-type counts, grouped and ordered
// for display. Internal datatypes (nigori, device_info, …) are omitted.
func (d *dynamoClient) SyncSummary(clientID string) ([]TypeCount, error) {
	byType := map[int]int{}
	var startKey json.RawMessage
	for page := 0; page < 60; page++ {
		body := map[string]any{
			"TableName":                 d.table,
			"KeyConditionExpression":    "ClientID = :c",
			"ExpressionAttributeValues": map[string]any{":c": map[string]string{"S": clientID}},
			"ProjectionExpression":      "#dt, #del",
			"ExpressionAttributeNames":  map[string]string{"#dt": "DataType", "#del": "Deleted"},
		}
		if len(startKey) > 0 {
			body["ExclusiveStartKey"] = startKey
		}
		payload, _ := json.Marshal(body)
		raw, err := d.call("DynamoDB_20120810.Query", payload)
		if err != nil {
			return nil, err
		}
		var out struct {
			Items            []map[string]json.RawMessage `json:"Items"`
			LastEvaluatedKey json.RawMessage              `json:"LastEvaluatedKey"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		for _, it := range out.Items {
			if del, ok := it["Deleted"]; ok {
				var b struct{ BOOL bool }
				if json.Unmarshal(del, &b) == nil && b.BOOL {
					continue
				}
			}
			dtRaw, ok := it["DataType"]
			if !ok {
				continue
			}
			var n struct{ N string }
			if json.Unmarshal(dtRaw, &n) != nil {
				continue
			}
			if dt, err := strconv.Atoi(n.N); err == nil {
				byType[dt]++
			}
		}
		if len(out.LastEvaluatedKey) == 0 || string(out.LastEvaluatedKey) == "null" {
			break
		}
		startKey = out.LastEvaluatedKey
	}

	// Aggregate raw datatype ids into friendly, display-ordered buckets.
	byName := map[string]int{}
	for dt, c := range byType {
		if name, ok := datatypeNames[dt]; ok {
			byName[name] += c
		}
	}
	var out []TypeCount
	for _, name := range datatypeDisplayOrder {
		if c := byName[name]; c > 0 {
			out = append(out, TypeCount{Name: name, Count: c})
			delete(byName, name)
		}
	}
	// Any remaining known-but-unordered names, largest first.
	var rest []TypeCount
	for name, c := range byName {
		rest = append(rest, TypeCount{Name: name, Count: c})
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Count > rest[j].Count })
	return append(out, rest...), nil
}

// datatypeNames maps go-sync's stored DataType (the Chromium EntitySpecifics
// field number) to a user-facing label. Several ids fold into one label.
// Anything not listed (nigori, device_info, experiments, user/security events,
// history_delete_directive, …) is intentionally hidden.
var datatypeNames = map[int]string{
	32904:   "Bookmarks",
	50119:   "Open tabs",
	963985:  "History",
	45873:   "Passwords",
	895275:  "Passkeys",
	63951:   "Addresses",
	1034378: "Contact info",
	306270:  "Payment methods",
	330441:  "Payment methods",
	1033580: "Payment methods",
	1164238: "Payment methods",
	31729:   "Autofill",
	411028:  "Reading list",
	48119:   "Extensions",
	96159:   "Extensions",
	88610:   "Search engines",
	41210:   "Themes",
	37702:   "Settings",
	163425:  "Settings",
	702141:  "OS settings",
	703915:  "OS settings",
	673225:  "Web apps",
	1117170: "Web apps",
	1004874: "Tab groups",
	1239418: "Tab groups",
	601980:  "Send-tab-to-self",
	170540:  "Spellcheck dictionary",
}

var datatypeDisplayOrder = []string{
	"Bookmarks", "Open tabs", "History", "Passwords", "Passkeys",
	"Addresses", "Payment methods", "Autofill", "Reading list", "Extensions",
	"Search engines", "Settings", "OS settings", "Themes", "Web apps",
	"Tab groups", "Send-tab-to-self", "Contact info", "Spellcheck dictionary",
}

// syncClientID derives an account's go-sync ClientID. It MUST match the gosync
// JWT patch: hex(sha256("ngp-client-id|" + subject)), subject = "x:<xid>".
func syncClientID(xid string) string {
	sum := sha256.Sum256([]byte("ngp-client-id|x:" + xid))
	return hex.EncodeToString(sum[:])
}
