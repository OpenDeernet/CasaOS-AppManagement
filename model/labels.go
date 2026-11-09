package model

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// What a user may set as a Docker label on a container (a service of a compose app, or the
// custom_labels of an app file). Labels are free text that reaches `docker create`, and some
// readers act on them: this box reads `casaos`, `origin` and `icon` to tell its own apps, and
// a reverse proxy such as Traefik routes by `traefik.*`. So:
//
//   - a key is 1 to 128 characters of letters, digits and `. _ - /`, starting with a letter or
//     a digit (no space, `=`, `:` or `$`: they cut a key short in the places that read one);
//   - a value is valid UTF-8 of at most 2048 bytes with no control character;
//   - at most 50 labels are added or changed per service in one request;
//   - the keys this box sets itself, and the namespaces of Docker Compose and of CasaOS, are
//     refused, so a label cannot turn a container into a CasaOS v1 app or take the place of
//     what Compose records about its project.
//
// Reverse-proxy labels (`traefik.*`) are allowed: setting them is why labels were asked for
// (CasaOS issue 1518), and only someone who can already write the whole compose file reaches
// this far. They are not filtered, so what they route is that user's to check.
const (
	MaxLabelKeyLength   = 128
	MaxLabelValueLength = 2048
	MaxLabelsPerService = 50
)

var labelKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// labels this box sets itself on a container, read back by service/container.go and v1/app.go
var reservedLabels = map[string]struct{}{
	"casaos": {}, "origin": {}, "icon": {}, "name": {}, "desc": {}, "host": {},
	"index": {}, "web": {}, "protocol": {}, "custom_id": {}, "show_env": {},
}

var reservedLabelPrefixes = []string{"io.casaos.", "com.docker.compose."}

// IsReservedLabel says whether the key is one this box or Docker Compose sets.
func IsReservedLabel(key string) bool {
	key = strings.ToLower(key)
	if _, ok := reservedLabels[key]; ok {
		return true
	}
	for _, prefix := range reservedLabelPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// ValidateLabel says why a label may not be set, or returns nil.
func ValidateLabel(key, value string) error {
	switch {
	case key == "":
		return fmt.Errorf("a label needs a name")
	case len(key) > MaxLabelKeyLength:
		return fmt.Errorf("label name %q is longer than %d characters", key, MaxLabelKeyLength)
	case !labelKey.MatchString(key):
		return fmt.Errorf("label name %q may only have letters, digits and . _ - /, and starts with a letter or a digit", key)
	case IsReservedLabel(key):
		return fmt.Errorf("label name %q is set by the system and cannot be used", key)
	case len(value) > MaxLabelValueLength:
		return fmt.Errorf("the value of label %q is longer than %d bytes", key, MaxLabelValueLength)
	case !utf8.ValidString(value):
		return fmt.Errorf("the value of label %q is not valid UTF-8", key)
	case strings.IndexFunc(value, unicode.IsControl) >= 0:
		return fmt.Errorf("the value of label %q has a control character", key)
	}
	return nil
}

// ValidateLabels checks labels a service gains or changes. known is what the service already
// carries (the installed app, or the store's version of it): a label equal to the one in known
// is not checked again, so an app that came with labels of its own, or with those this box
// added, can be saved as it is. nil known checks them all.
func ValidateLabels(labels, known map[string]string) error {
	keys := make([]string, 0, len(labels))
	for key, value := range labels {
		if old, ok := known[key]; ok && old == value {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	if len(keys) > MaxLabelsPerService {
		return fmt.Errorf("%d labels added or changed, at most %d are accepted", len(keys), MaxLabelsPerService)
	}

	for _, key := range keys {
		if err := ValidateLabel(key, labels[key]); err != nil {
			return err
		}
	}
	return nil
}
