package telegram

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"testing"
	"time"
)

func signWebApp(botToken, query string) string {
	pairs := []string{}
	for _, p := range splitAmp(query) {
		if p == "" {
			continue
		}
		pairs = append(pairs, p)
	}
	// caller passes already-sorted check string via query without hash
	h := hmac.New(sha256.New, []byte(WebAppDataKey))
	h.Write([]byte(botToken))
	key := h.Sum(nil)
	h = hmac.New(sha256.New, key)
	h.Write([]byte(queryToCheckString(query)))
	return hex.EncodeToString(h.Sum(nil))
}

func splitAmp(s string) []string {
	out := []string{}
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == '&' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(s[i])
	}
	out = append(out, cur)
	return out
}

func queryToCheckString(query string) string {
	// validateWebAppData sorts keys and unescapes
	type kv struct{ k, v string }
	var list []kv
	for _, p := range splitAmp(query) {
		i := indexByte(p, '=')
		if i < 0 {
			continue
		}
		k, v := p[:i], p[i+1:]
		if k == "hash" {
			continue
		}
		list = append(list, kv{k, v})
	}
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if list[j].k < list[i].k {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	out := ""
	for i, p := range list {
		if i > 0 {
			out += "\n"
		}
		val, _ := url.QueryUnescape(p.v)
		out += p.k + "=" + val
	}
	return out
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func TestValidateWebAppDataOK(t *testing.T) {
	token := "test-bot-token"
	user := url.QueryEscape(`{"id":1,"username":"alice"}`)
	auth := fmt.Sprintf("%d", time.Now().Unix())
	raw := "auth_date=" + auth + "&user=" + user
	hash := signWebApp(token, raw)
	got, err := validateWebAppData(token, raw+"&hash="+hash)
	if err != nil {
		t.Fatal(err)
	}
	if got["auth_date"] == nil {
		t.Fatal("missing auth_date")
	}
}

func TestValidateWebAppDataBadHash(t *testing.T) {
	_, err := validateWebAppData("tok", "auth_date=1&hash=deadbeef")
	if err == nil {
		t.Fatal("expected invalid hash")
	}
}

func TestValidateWebAppDataExpired(t *testing.T) {
	token := "tok"
	auth := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	raw := "auth_date=" + auth
	hash := signWebApp(token, raw)
	_, err := validateWebAppData(token, raw+"&hash="+hash)
	if err == nil {
		t.Fatal("expected expired")
	}
}
