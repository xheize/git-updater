package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// Zot 2.x emits CloudEvents. Its source is an event producer URI, not a
// registry hostname; never use the HTTP Host header to build an image name.
type zotCloudEvent struct {
	Version string          `json:"specversion"`
	ID      string          `json:"id"`
	Source  string          `json:"source"`
	Type    string          `json:"type"`
	Time    string          `json:"time,omitempty"`
	Data    json.RawMessage `json:"data"`
}

func decodeZotEvent(c *fiber.Ctx) (ZotWebhookPayload, string, string, error) {
	var p ZotWebhookPayload
	media, _, _ := mime.ParseMediaType(c.Get("Content-Type"))
	binary := false
	c.Request().Header.VisitAll(func(k, v []byte) {
		if strings.HasPrefix(strings.ToLower(string(k)), "ce-") {
			binary = true
		}
	})
	structured := media == "application/cloudevents+json"
	if !binary && !structured {
		// Reject a structured envelope mislabeled as legacy JSON.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(c.Body(), &probe); err != nil {
			return p, "", "", err
		}
		if _, ok := probe["specversion"]; ok {
			return p, "", "", fmt.Errorf("structured CloudEvents require application/cloudevents+json")
		}
		err := c.BodyParser(&p)
		return p, "zot:" + p.Request.Host, webhookPayloadHash(c.Body()), err
	}
	if binary && structured {
		return p, "", "", fmt.Errorf("mixed CloudEvents encodings")
	}
	e := zotCloudEvent{}
	if structured {
		if err := json.Unmarshal(c.Body(), &e); err != nil {
			return p, "", "", err
		}
	} else {
		e = zotCloudEvent{Version: c.Get("ce-specversion"), ID: c.Get("ce-id"), Source: c.Get("ce-source"), Type: c.Get("ce-type"), Time: c.Get("ce-time"), Data: json.RawMessage(c.Body())}
	}
	if e.Version != "1.0" || strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.Source) == "" || strings.TrimSpace(e.Type) == "" {
		return p, "", "", fmt.Errorf("CloudEvents 1.0 specversion, id, source and type are required")
	}
	if _, err := url.Parse(e.Source); err != nil {
		return p, "", "", fmt.Errorf("invalid CloudEvents source")
	}
	p.ID = e.ID
	if e.Time != "" {
		var err error
		p.Timestamp, err = time.Parse(time.RFC3339Nano, e.Time)
		if err != nil {
			return p, "", "", fmt.Errorf("invalid CloudEvents time")
		}
	}
	if e.Type != "zotregistry.image.updated" {
		p.Action = e.Type
		return p, "", "", nil
	}
	var data struct {
		Name      string `json:"name"`
		Reference string `json:"reference"`
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return p, "", "", fmt.Errorf("invalid CloudEvents data")
	}
	if data.Name == "" || data.Reference == "" {
		return p, "", "", fmt.Errorf("CloudEvents name and reference are required")
	}
	if strings.ContainsAny(data.Name, ":@") || strings.HasPrefix(data.Name, "/") {
		return p, "", "", fmt.Errorf("invalid Zot repository name")
	}
	// Digest-addressed manifest uploads are not tag updates.
	if strings.Contains(data.Reference, ":") {
		p.Action = "digest_upload"
		return p, "", "", nil
	}
	host := strings.TrimSpace(os.Getenv("ZOT_REGISTRY_HOST"))
	if host == "" {
		hosts := strings.Split(os.Getenv("REGISTRY_HOSTS"), ",")
		if len(hosts) == 1 {
			host = strings.TrimSpace(hosts[0])
		}
	}
	u, err := url.Parse("https://" + host)
	if err != nil || host == "" || u.Host != host || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return p, "", "", fmt.Errorf("ZOT_REGISTRY_HOST or a single REGISTRY_HOSTS entry is required")
	}
	p.Action = "push"
	p.Request.Host = host
	p.Target.Repository, p.Target.Tag = data.Name, data.Reference
	p.Target.Digest, p.Target.MediaType = data.Digest, data.MediaType
	scope, _ := json.Marshal([]string{"zot-cloudevents", host, e.Source})
	// Include binary-mode metadata in the deduplication hash. Hashing the body
	// alone would miss a changed event type/time with the same delivery ID.
	canonical, err := json.Marshal(e)
	if err != nil {
		return p, "", "", err
	}
	return p, string(scope), webhookPayloadHash(canonical), nil
}
