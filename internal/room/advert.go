package room

import (
	"log/slog"
	"time"

	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
)

// BuildAdvert returns the signed ROOM advert packet for a room: a flood advert
// in the default scope, or a zero-hop advert that repeaters do not forward.
func BuildAdvert(id *mesh.Identity, cfg models.RoomConfig, meshCfg models.MeshConfig, timestamp uint32, flood bool) (mesh.Packet, error) {
	info := mesh.AdvertInfo{Type: mesh.AdvertTypeRoom, Name: cfg.Name}
	if cfg.Location != nil {
		info.HasLocation = true
		info.Latitude, info.Longitude = cfg.Location.Latitude, cfg.Location.Longitude
	}
	appData, err := info.AppData()
	if err != nil {
		return mesh.Packet{}, err
	}
	payload, err := mesh.AdvertPayload(id, timestamp, appData)
	if err != nil {
		return mesh.Packet{}, err
	}
	if !flood {
		return mesh.NewZeroHopPacket(mesh.PayloadAdvert, payload), nil
	}
	var scope *mesh.TransportKey
	if meshCfg.DefaultScope != "" {
		key := mesh.RegionKey(meshCfg.DefaultScope)
		scope = &key
	}
	return mesh.NewFloodPacket(mesh.PayloadAdvert, payload, meshCfg.PathHashSize, scope)
}

// tickAdverts sends the startup advert and then the periodic ones: a flood
// advert every flood interval and a zero-hop advert every local interval. A
// flood advert restarts the local timer so the two do not overlap, as in the
// reference. Startup sends one flood advert (or a zero-hop one when flooding is
// off), staggered across rooms.
func (r *roomState) tickAdverts(now time.Time) {
	flood := time.Duration(r.cfg.FloodAdvertIntervalHours) * time.Hour
	local := time.Duration(r.cfg.AdvertIntervalMin) * time.Minute
	if !r.startupSent {
		if now.Before(r.startupAt) {
			return
		}
		r.startupSent = true
		if flood > 0 {
			r.sendAdvert(now, true)
			r.nextFloodAdvert = now.Add(flood)
		} else if local > 0 {
			r.sendAdvert(now, false)
		}
		if local > 0 {
			r.nextLocalAdvert = now.Add(local)
		}
		return
	}
	switch {
	case !r.nextFloodAdvert.IsZero() && !now.Before(r.nextFloodAdvert):
		r.sendAdvert(now, true)
		r.nextFloodAdvert = now.Add(flood)
		if local > 0 {
			r.nextLocalAdvert = now.Add(local)
		}
	case !r.nextLocalAdvert.IsZero() && !now.Before(r.nextLocalAdvert):
		r.sendAdvert(now, false)
		r.nextLocalAdvert = now.Add(local)
	}
}

func (r *roomState) sendAdvert(now time.Time, flood bool) {
	pkt, err := BuildAdvert(r.id, r.cfg, r.svc.mesh, r.svc.uniqueNow(now), flood)
	if err != nil {
		slog.Error("[Room] cannot build advert", "room", r.cfg.ID, "error", err)
		return
	}
	tag := "advert:" + r.cfg.ID
	r.svc.tx.Cancel(tag) // a queued stale advert is replaced, not stacked
	r.svc.submit(pkt, kiss.TxPriorityAdvert, 0, tag, r.cfg.ID)
}
