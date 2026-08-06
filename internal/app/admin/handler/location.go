package handler

import (
	"database/sql"
	"net/url"

	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// locationPreview is the single value the map_preview partial receives.
//
// A struct built here rather than assembled in the template: a partial takes
// exactly one value, there is no dict helper, and adding one is declined. Its
// own file for the same reason the public locationPicker has one — the payment
// and user detail pages will want the same block, and a copy in each would be
// one edit away from disagreeing about what the map is configured with.
type locationPreview struct {
	// Latitude and Longitude are the stored pin, as strings. Nothing here parses
	// them: they are DECIMAL(9,7)/DECIMAL(10,7) and money's rule applies — the
	// value shown, linked and stored must be the same characters.
	Latitude  string
	Longitude string
	// MapsHref opens the pin in Google Maps. Built here so the escaping is Go's,
	// not a template author's.
	MapsHref string
	// Label is the copy under the map. Optional.
	Label string
	// Map is the tile server and fallback view, from MAP_TILE_URL and friends —
	// the same setting the Content-Security-Policy's img-src is derived from.
	Map view.MapSettings
}

// locationPreviewFor assembles the payload from a stored coordinate pair.
//
// ok is false when there is no pin, which is the ordinary case: the map picker
// is optional on both the booking form and the profile, so most rows have none.
// The caller renders nothing rather than a map of nowhere.
//
// Both or neither, never half — coordinatePair enforces that on the way in, and
// this mirrors it on the way out so a half-written pair cannot produce a link to
// the equator.
func locationPreviewFor(deps *app.Deps, lat, lng sql.NullString, label string) (locationPreview, bool) {
	if !lat.Valid || !lng.Valid || lat.String == "" || lng.String == "" {
		return locationPreview{}, false
	}

	return locationPreview{
		Latitude:  lat.String,
		Longitude: lng.String,
		// The documented cross-platform form: it opens the app on Android and
		// iOS and the website everywhere else.
		MapsHref: "https://www.google.com/maps/search/?api=1&query=" +
			url.QueryEscape(lat.String+","+lng.String),
		Label: label,
		Map: view.MapSettings{
			TileURL:     deps.Cfg.Map.TileURL,
			Attribution: deps.Cfg.Map.TileAttribution,
			DefaultLat:  deps.Cfg.Map.DefaultLat,
			DefaultLng:  deps.Cfg.Map.DefaultLng,
			DefaultZoom: deps.Cfg.Map.DefaultZoom,
		},
	}, true
}
