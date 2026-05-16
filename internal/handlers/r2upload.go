package handlers

// r2upload.go — minimal S3-compatible PutObject for Cloudflare R2.
//
// Uses only the Go standard library (net/http, crypto/hmac, crypto/sha256,
// encoding/hex). No external AWS SDK is required.
//
// R2 endpoint: https://<accountID>.r2.cloudflarestorage.com
// The request is signed with AWS Signature Version 4 (service = "s3").

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// r2Client holds credentials for one R2 bucket.
type r2Client struct {
	accountID string // Cloudflare account ID (hex string in the subdomain)
	accessKey string // R2 Access Key ID
	secretKey string // R2 Secret Access Key
	bucket    string // bucket name
	publicURL string // public CDN/custom domain, e.g. https://cdn.example.com
}

// PutObject uploads body to key inside the bucket and returns the public URL.
// contentType should be the MIME type, e.g. "image/jpeg".
func (r *r2Client) PutObject(key, contentType string, body []byte) (string, error) {
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", r.accountID)
	path := fmt.Sprintf("/%s/%s", r.bucket, key)
	rawURL := endpoint + path

	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	payloadHash := hashSHA256(body)

	req, err := http.NewRequest(http.MethodPut, rawURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("r2: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", amzDate)

	// Canonical headers (must be sorted, lowercase).
	signedHeaders, canonicalHeaders := buildCanonicalHeaders(req)

	canonicalRequest := strings.Join([]string{
		http.MethodPut,
		path,
		"", // no query string
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credScope := fmt.Sprintf("%s/auto/s3/aws4_request", dateStamp)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credScope,
		hashSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKey(r.secretKey, dateStamp, "auto", "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		r.accessKey, credScope, signedHeaders, signature,
	)
	req.Header.Set("Authorization", authHeader)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("r2: upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated &&
		resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("r2: upload failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	// Return via public URL (custom domain / CDN) if configured, else fall back
	// to the authenticated R2 endpoint.
	publicBase := strings.TrimRight(r.publicURL, "/")
	if publicBase == "" {
		publicBase = endpoint + "/" + r.bucket
	}
	return fmt.Sprintf("%s/%s", publicBase, key), nil
}

// ── SigV4 helpers ────────────────────────────────────────────────────────────

func hashSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// buildCanonicalHeaders returns (signedHeaders, canonicalHeadersBlock).
// The canonical block ends with a blank line as required by SigV4.
func buildCanonicalHeaders(req *http.Request) (signedHeaders, canonicalHeaders string) {
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		pairs = append(pairs, kv{lk, strings.TrimSpace(strings.Join(vs, ","))})
	}
	// host is not in req.Header but must be included
	pairs = append(pairs, kv{"host", req.Host})
	if req.Host == "" {
		pairs = append(pairs, kv{"host", req.URL.Host})
		// remove duplicate we just added
		clean := pairs[:0]
		seen := map[string]bool{}
		for _, p := range pairs {
			if !seen[p.k] {
				clean = append(clean, p)
				seen[p.k] = true
			}
		}
		pairs = clean
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].k < pairs[j].k })

	var sb strings.Builder
	var keys []string
	for _, p := range pairs {
		if p.k == "host" {
			sb.WriteString(fmt.Sprintf("host:%s\n", req.URL.Host))
		} else {
			sb.WriteString(fmt.Sprintf("%s:%s\n", p.k, p.v))
		}
		keys = append(keys, p.k)
	}
	return strings.Join(keys, ";"), sb.String()
}
