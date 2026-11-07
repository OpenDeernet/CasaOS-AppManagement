package service

import (
	"fmt"
	"net/http"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"go.uber.org/zap"
)

// The catalogue's mirror.
//
// The App Store is IceWhale's catalogue, fetched as a zip: since February 2025
// the artifact IceWhaleTech/CasaOS-AppStore builds on its gh-pages branch,
// served through jsdelivr, and before that a zip of the IceWhaleTech/_appstore
// repository, which a box installed then and upgraded since may still name in
// its configuration. The day the source goes away, every box keeps the copy it
// already has and never sees an update again, and a fresh install gets the seed
// the installer ships and nothing after. This distribution keeps a copy of the
// artifact, refreshed every night (github.com/ReCasaOS/_appstore); when the
// original stops answering, the copy is fetched instead. The configured URL is
// not changed, on disk or in the dashboard: the original is still the source,
// and it is tried first on every update.

// mirrorZip is this distribution's copy of the catalogue: the same bytes as
// IceWhale's store/main.zip, taken every night.
const mirrorZip = "https://raw.githubusercontent.com/ReCasaOS/_appstore/main/store/main.zip"

// recasaosCatalogue is the store a ReCasaOS box names by default: IceWhale's
// apps, RoqueOS's and our Big Bear fork's merged every night.
const recasaosCatalogue = "https://raw.githubusercontent.com/ReCasaOS/_appstore/catalogue/store/recasaos.zip"

// catalogueMirrors maps a catalogue's URL to this distribution's copy of it.
var catalogueMirrors = map[string]string{
	// the URL every configuration written since February 2025 carries
	"https://cdn.jsdelivr.net/gh/IceWhaleTech/CasaOS-AppStore@gh-pages/store/main.zip": mirrorZip,
	// the URL a box installed before that, and upgraded, still carries
	"https://github.com/IceWhaleTech/_appstore/archive/refs/heads/main.zip": mirrorZip,
	// the ReCasaOS catalogue (github.com/ReCasaOS/_appstore, branch catalogue): the same bytes through jsdelivr
	recasaosCatalogue: "https://cdn.jsdelivr.net/gh/ReCasaOS/_appstore@catalogue/store/recasaos.zip",
}

// pickCatalogueSource asks the catalogue's URL whether it answers, and its
// mirror when it does not. It returns the URL to fetch and the answer, whose
// ContentLength says whether anything changed.
func pickCatalogueSource(url string, head func(string) (*http.Response, error)) (string, *http.Response, error) {
	res, err := head(url)
	if err == nil && res.StatusCode == http.StatusOK {
		return url, res, nil
	}

	if err == nil {
		err = fmt.Errorf("failed to get appstore size, status code: %d", res.StatusCode)
	}

	mirror, ok := catalogueMirrors[url]
	if !ok {
		return "", nil, err
	}

	logger.Warn("the catalogue does not answer; trying this distribution's copy of it",
		zap.String("url", url), zap.String("mirror", mirror), zap.Error(err))

	res, mirrorErr := head(mirror)
	if mirrorErr != nil {
		return "", nil, fmt.Errorf("%w (and the mirror: %v)", err, mirrorErr)
	}
	if res.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("%w (and the mirror answered %d)", err, res.StatusCode)
	}

	return mirror, res, nil
}
