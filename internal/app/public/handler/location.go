package handler

import (
	"github.com/remorac/appskep-tpj/internal/app"
	"github.com/remorac/appskep-tpj/internal/shared/view"
)

// locationPicker is the single value the map_picker partial receives.
//
// A struct built here rather than assembled in the template: a partial takes
// exactly one value, there is no dict helper, and adding one is declined. It
// lives in its own file because two pages in this package build it — the booking
// form and the profile form — and a copy in each would be one edit away from
// disagreeing about what the map is configured with.
type locationPicker struct {
	// Latitude and Longitude are the submitted or stored pin, as strings, empty
	// when there is none. The template writes them into hidden inputs and the
	// script writes them back; nothing here ever parses them.
	Latitude  string
	Longitude string
	// Error is the message for the "lokasi" key, if the submission carried a
	// broken pin. Keyed to the map card rather than to either hidden input,
	// because a hidden input has nowhere to render a message.
	Error string
	// Help is the copy under the map when there is no error. It differs between
	// the two pages: on the booking form the pin is for this visit, on the
	// profile it is a default for the next one.
	Help string
	// Map is the tile server and starting view, from MAP_TILE_URL and friends.
	// The same setting the Content-Security-Policy's img-src is derived from.
	Map view.MapSettings
}

// locationPickerFor assembles the payload from a form's coordinate pair.
//
// errs is the whole ValidationError map, looked up here so neither caller has to
// remember which key the coordinate messages use.
func locationPickerFor(deps *app.Deps, lat, lng, help string, errs map[string]string) locationPicker {
	return locationPicker{
		Latitude:  lat,
		Longitude: lng,
		Error:     errs["lokasi"],
		Help:      help,
		Map: view.MapSettings{
			TileURL:     deps.Cfg.Map.TileURL,
			Attribution: deps.Cfg.Map.TileAttribution,
			DefaultLat:  deps.Cfg.Map.DefaultLat,
			DefaultLng:  deps.Cfg.Map.DefaultLng,
			DefaultZoom: deps.Cfg.Map.DefaultZoom,
		},
	}
}
