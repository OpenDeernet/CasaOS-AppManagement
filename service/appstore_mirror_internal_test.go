package service

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
)

const iceWhale = "https://cdn.jsdelivr.net/gh/IceWhaleTech/CasaOS-AppStore@gh-pages/store/main.zip"

func answering(code int, size int64) func(string) (*http.Response, error) {
	return func(string) (*http.Response, error) {
		return &http.Response{StatusCode: code, ContentLength: size}, nil
	}
}

func TestTheOriginalIsTheSourceWhileItAnswers(t *testing.T) {
	logger.LogInitConsoleOnly()
	source, res, err := pickCatalogueSource(iceWhale, answering(http.StatusOK, 42))
	if err != nil || source != iceWhale || res.ContentLength != 42 {
		t.Fatalf("got %q %v %v", source, res, err)
	}
}

func TestTheCopyIsTheSourceWhenTheOriginalIsGone(t *testing.T) {
	logger.LogInitConsoleOnly()
	asked := []string{}
	head := func(url string) (*http.Response, error) {
		asked = append(asked, url)
		if url == iceWhale {
			return &http.Response{StatusCode: http.StatusNotFound}, nil
		}

		return &http.Response{StatusCode: http.StatusOK, ContentLength: 7}, nil
	}

	source, res, err := pickCatalogueSource(iceWhale, head)
	if err != nil || !strings.Contains(source, "ReCasaOS/_appstore") || res.ContentLength != 7 {
		t.Fatalf("got %q %v %v", source, res, err)
	}
	if len(asked) != 2 || asked[0] != iceWhale {
		t.Fatalf("the original is asked first, then the copy: %v", asked)
	}
}

func TestACatalogueWithNoCopyFailsAsItAlwaysDid(t *testing.T) {
	logger.LogInitConsoleOnly()
	down := func(string) (*http.Response, error) { return nil, errors.New("no route to host") }
	if _, _, err := pickCatalogueSource("https://example.org/store.zip", down); err == nil || !strings.Contains(err.Error(), "no route") {
		t.Fatalf("want the original error, got %v", err)
	}
}

func TestTheOldStoreURLHasTheSameCopy(t *testing.T) {
	logger.LogInitConsoleOnly()
	head := func(url string) (*http.Response, error) {
		if strings.Contains(url, "IceWhaleTech") {
			return &http.Response{StatusCode: http.StatusNotFound}, nil
		}

		return &http.Response{StatusCode: http.StatusOK, ContentLength: 1}, nil
	}
	source, _, err := pickCatalogueSource("https://github.com/IceWhaleTech/_appstore/archive/refs/heads/main.zip", head)
	if err != nil || source != mirrorZip {
		t.Fatalf("a box that still names the old repository gets the copy too: %q %v", source, err)
	}
}

func TestBothGoneIsOneErrorNamingBoth(t *testing.T) {
	logger.LogInitConsoleOnly()
	down := func(string) (*http.Response, error) { return nil, errors.New("no route to host") }
	_, _, err := pickCatalogueSource(iceWhale, down)
	if err == nil || !strings.Contains(err.Error(), "mirror") {
		t.Fatalf("want an error that names the mirror too, got %v", err)
	}
}

func TestTheRecasaosCatalogueHasACopyOnAnotherHost(t *testing.T) {
	logger.LogInitConsoleOnly()
	head := func(url string) (*http.Response, error) {
		if url == recasaosCatalogue {
			return &http.Response{StatusCode: http.StatusBadGateway}, nil
		}

		return &http.Response{StatusCode: http.StatusOK, ContentLength: 9}, nil
	}

	source, _, err := pickCatalogueSource(recasaosCatalogue, head)
	if err != nil || !strings.Contains(source, "jsdelivr") || !strings.Contains(source, "@catalogue/store/recasaos.zip") {
		t.Fatalf("got %q %v", source, err)
	}
}
