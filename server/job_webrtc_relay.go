package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/livepeer/go-livepeer/clog"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// WHIPSession represents a WHIP ingestion session
type WHIPSession struct {
	ID         string
	PeerConn   *webrtc.PeerConnection
	Tracks     map[string]*webrtc.TrackRemote
	mu         sync.RWMutex
	Created    time.Time
	LastUpdate time.Time
}

// WHEPSession represents a WHEP egress session
type WHEPSession struct {
	ID          string
	PeerConn    *webrtc.PeerConnection
	LocalTracks map[string]*webrtc.TrackLocalStaticRTP // Changed from Transceivers
	mu          sync.RWMutex
	Created     time.Time
	LastUpdate  time.Time
}

// RelayServer manages WHIP/WHEP connections and media relay
type RelayServer struct {
	ctx             context.Context
	ingress         *WHIPSession
	egress          []*WHEPSession
	releaseCapacity chan struct{}

	mu         sync.RWMutex
	transforms map[string]func([]byte) []byte
	stats      *RelayStats
}

// RelayStats tracks relay statistics
type RelayStats struct {
	PacketsReceived    int64         `json:"packets_received"`
	PacketsTransmitted int64         `json:"packets_transmitted"`
	BytesReceived      int64         `json:"bytes_received"`
	BytesTransmitted   int64         `json:"bytes_transmitted"`
	TransformTime      time.Duration `json:"transform_time"`
	mu                 sync.RWMutex
}

func NewRelayServer(ctx context.Context) *RelayServer {
	rs := &RelayServer{
		ctx:        ctx,
		ingress:    &WHIPSession{},
		egress:     []*WHEPSession{},
		transforms: make(map[string]func([]byte) []byte),
		stats:      &RelayStats{},
	}

	go rs.Start()

	// Add transformations if applicable
	// e.g. time stamp correction, audio/video preprocessing, etc
	//rs.transforms["audio"] = rs.transformAudio
	//rs.transforms["video"] = rs.transformVideo

	return rs
}

func (rs *RelayServer) Start() {
	<-rs.ctx.Done()
	rs.stop()
}

func (rs *RelayServer) stop() {
	clog.Infof(rs.ctx, "RelayServer: Shutting down")
	rs.cleanupWHIPSession()
	rs.cleanupWHEPSessions()

	//release the capacity for source stream
	if rs.releaseCapacity != nil {
		clog.Infof(rs.ctx, "RelayServer: Releasing capacity for source stream")
		rs.releaseCapacity <- struct{}{}
	}
}

func (rs *RelayServer) CreateWHIPSession(body []byte, contentType string, streamID string, sessionType string) (string, string, int, error) {
	// Validate Content-Type
	if contentType != "application/sdp" {
		return "", "", http.StatusBadRequest, errors.New("Content-Type must be application/sdp")
	}

	// Create WebRTC peer connection
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	peerConn, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return "", "", http.StatusInternalServerError, errors.New(fmt.Sprintf("Failed to create peer connection: %v", err))
	}

	// Set handler for ICE candidates
	peerConn.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			// Gathering done
			clog.Infof(rs.ctx, "ICE candidate gathering complete")
			return
		}
		clog.Infof(rs.ctx, "New ICE Candidate: %s", candidate.ToJSON().Candidate)
	})

	// Generate session ID
	sessionID := streamID + "_" + sessionType

	session := &WHIPSession{
		ID:         sessionID,
		PeerConn:   peerConn,
		Tracks:     make(map[string]*webrtc.TrackRemote),
		Created:    time.Now(),
		LastUpdate: time.Now(),
	}

	// Handle incoming tracks
	peerConn.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		clog.Infof(rs.ctx, "WHIP Session %s: Received track %s", sessionID, track.Kind().String())

		session.mu.Lock()
		session.Tracks[track.Kind().String()] = track
		session.mu.Unlock()

		// Create local tracks for existing WHEP sessions when new tracks arrive
		rs.createLocalTracksForExistingWHEPSessions(track)

		// Start relaying and transforming this track
		go rs.relayAndTransformTrack(sessionID, track, "whip")
	})

	// Handle connection state changes
	peerConn.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		clog.Infof(rs.ctx, "WHIP Session %s: Connection state changed to %s", sessionID, state.String())
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			//cleanup WHIP and WHEP sessions
			rs.ctx.Done()
		}
	})

	// Set remote description (offer)
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(body),
	}

	err = peerConn.SetRemoteDescription(offer)
	if err != nil {
		return "", "", http.StatusBadRequest, errors.New(fmt.Sprintf("Failed to set remote description: %v", err))
	}

	// Create answer
	answer, err := peerConn.CreateAnswer(nil)
	if err != nil {
		return "", "", http.StatusInternalServerError, errors.New(fmt.Sprintf("Failed to create answer: %v", err))
	}

	// Gather ICE candidates and set local description
	gatherComplete := webrtc.GatheringCompletePromise(peerConn)
	if err = peerConn.SetLocalDescription(answer); err != nil {
		e := fmt.Sprintf("SetLocalDescription failed: %v", err)
		return "", "", http.StatusInternalServerError, errors.New(e)
	}
	// Wait for ICE gathering if you want the full candidate set in the SDP
	<-gatherComplete
	//set the full answer after ICE candidates
	answer = *peerConn.LocalDescription()

	// Store session
	rs.mu.Lock()
	rs.ingress = session
	rs.mu.Unlock()

	clog.Infof(rs.ctx, "Created WHIP session: %s", sessionID)

	return sessionID, answer.SDP, http.StatusCreated, nil
}

func (rs *RelayServer) CreateWHEPSession(body []byte, contentType string, streamID string, sessionType string) (string, string, int, error) {
	if contentType != "application/sdp" {
		return "", "", http.StatusBadRequest, errors.New("Content-Type must be application/sdp")
	}

	// Create WebRTC peer connection
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	peerConn, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return "", "", http.StatusInternalServerError, errors.New(fmt.Sprintf("Failed to create peer connection: %v", err))
	}

	// Set handler for ICE candidates
	peerConn.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			// Gathering done
			clog.Infof(rs.ctx, "ICE candidate gathering complete")
			return
		}
		clog.Infof(rs.ctx, "New ICE Candidate: %s", candidate.ToJSON().Candidate)
	})

	// Generate session ID
	sessionID := streamID + "_" + sessionType

	session := &WHEPSession{
		ID:          sessionID,
		PeerConn:    peerConn,
		LocalTracks: make(map[string]*webrtc.TrackLocalStaticRTP), // Initialize LocalTracks
		Created:     time.Now(),
		LastUpdate:  time.Now(),
	}

	// Create and add tracks for available media from ingress
	err = rs.addTracksToWHEPSession(session)
	if err != nil {
		return "", "", http.StatusInternalServerError, errors.New(fmt.Sprintf("Failed to add tracks to WHEP session: %v", err))
	}

	// Handle connection state changes
	peerConn.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		clog.Infof(rs.ctx, "WHEP Session %s: Connection state changed to %s", sessionID, state.String())
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed {
			rs.cleanupWHEPSession(sessionID)
		}
	})

	// Set remote description (offer)
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(body),
	}

	err = peerConn.SetRemoteDescription(offer)
	if err != nil {
		return "", "", http.StatusBadRequest, errors.New(fmt.Sprintf("Failed to set remote description: %v", err))
	}

	// Create answer
	answer, err := peerConn.CreateAnswer(nil)
	if err != nil {
		return "", "", http.StatusInternalServerError, errors.New(fmt.Sprintf("Failed to create answer: %v", err))
	}

	// Gather ICE candidates and set local description
	gatherComplete := webrtc.GatheringCompletePromise(peerConn)
	if err = peerConn.SetLocalDescription(answer); err != nil {
		e := fmt.Sprintf("SetLocalDescription failed: %v", err)
		return "", "", http.StatusInternalServerError, errors.New(e)
	}
	// Wait for ICE gathering if you want the full candidate set in the SDP
	<-gatherComplete
	//set the full answer after ICE candidates
	answer = *peerConn.LocalDescription()

	// Store session
	rs.mu.Lock()
	rs.egress = append(rs.egress, session)
	rs.mu.Unlock()

	clog.Infof(rs.ctx, "Created WHEP session: %s", sessionID)

	return sessionID, answer.SDP, http.StatusCreated, nil
}

// Updated to create TrackLocalStaticRTP tracks based on ingress tracks
func (rs *RelayServer) addTracksToWHEPSession(session *WHEPSession) error {
	ingressTracks := rs.ingress.Tracks

	// If we have ingress tracks, create matching local tracks
	for trackKind, remoteTrack := range ingressTracks {
		err := rs.createLocalTrackForWHEPSession(session, trackKind, remoteTrack)
		if err != nil {
			return fmt.Errorf("failed to create local track for %s: %v", trackKind, err)
		}
	}

	// If no ingress tracks yet, create generic tracks
	if len(ingressTracks) == 0 {
		// Create generic audio track
		err := rs.createGenericLocalTrack(session, webrtc.RTPCodecTypeAudio)
		if err != nil {
			return fmt.Errorf("failed to create generic audio track: %v", err)
		}

		// Create generic video track
		err = rs.createGenericLocalTrack(session, webrtc.RTPCodecTypeVideo)
		if err != nil {
			return fmt.Errorf("failed to create generic video track: %v", err)
		}
	}

	return nil
}

// Create a local track based on remote track information
func (rs *RelayServer) createLocalTrackForWHEPSession(session *WHEPSession, trackKind string, remoteTrack *webrtc.TrackRemote) error {
	// Get codec information from the remote track
	codec := remoteTrack.Codec()

	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		codec.RTPCodecCapability,
		trackKind,
		fmt.Sprintf("relay-%s", session.ID),
	)
	if err != nil {
		return fmt.Errorf("failed to create local track: %v", err)
	}

	// Add track to peer connection
	_, err = session.PeerConn.AddTrack(localTrack)
	if err != nil {
		return fmt.Errorf("failed to add track to peer connection: %v", err)
	}

	// Store the local track
	session.LocalTracks[trackKind] = localTrack

	clog.Infof(rs.ctx, "Created local track for WHEP session %s: %s (%s)", session.ID, trackKind, codec.MimeType)
	return nil
}

// Create generic local tracks when no ingress is available
func (rs *RelayServer) createGenericLocalTrack(session *WHEPSession, codecType webrtc.RTPCodecType) error {
	var capability webrtc.RTPCodecCapability
	var trackKind string

	switch codecType {
	case webrtc.RTPCodecTypeAudio:
		capability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}
		trackKind = "audio"
	case webrtc.RTPCodecTypeVideo:
		capability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}
		trackKind = "video"
	default:
		return fmt.Errorf("unsupported codec type: %v", codecType)
	}

	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		capability,
		trackKind,
		fmt.Sprintf("relay-%s", session.ID),
	)
	if err != nil {
		return fmt.Errorf("failed to create local track: %v", err)
	}

	// Add track to peer connection
	_, err = session.PeerConn.AddTrack(localTrack)
	if err != nil {
		return fmt.Errorf("failed to add track to peer connection: %v", err)
	}

	// Store the local track
	session.LocalTracks[trackKind] = localTrack

	clog.Infof(rs.ctx, "Created generic local track for WHEP session %s: %s (%s)", session.ID, trackKind, capability.MimeType)
	return nil
}

// Create local tracks for existing WHEP sessions when new ingress tracks arrive
func (rs *RelayServer) createLocalTracksForExistingWHEPSessions(remoteTrack *webrtc.TrackRemote) {
	trackKind := remoteTrack.Kind().String()

	rs.mu.RLock()
	for _, session := range rs.egress {
		session.mu.Lock()
		// Check if we already have this track type
		if _, exists := session.LocalTracks[trackKind]; !exists {
			err := rs.createLocalTrackForWHEPSession(session, trackKind, remoteTrack)
			if err != nil {
				clog.Infof(rs.ctx, "Failed to create local track for existing WHEP session %s: %v", session.ID, err)
			}
		}
		session.mu.Unlock()
	}
	rs.mu.RUnlock()
}

// Updated relay function to use TrackLocalStaticRTP
func (rs *RelayServer) relayToWHEPSessions(rtp *rtp.Packet, trackKind string) {
	rs.mu.RLock()

	for _, session := range rs.egress {
		session.mu.RLock()
		if localTrack, exists := session.LocalTracks[trackKind]; exists {
			err := localTrack.WriteRTP(rtp)
			if err != nil {
				clog.Infof(rs.ctx, "Error writing RTP to WHEP session %s: %v", session.ID, err)
			} else {
				rs.stats.mu.Lock()
				rs.stats.PacketsTransmitted++
				rs.stats.BytesTransmitted += int64(len(rtp.Payload))
				rs.stats.mu.Unlock()
			}
		} else {
			clog.Infof(rs.ctx, "Local track %s not found in WHEP session %s", trackKind, session.ID)
		}
		session.mu.RUnlock()
	}
	rs.mu.RUnlock()
}

func (rs *RelayServer) updateWHIPSession(sessionID string, body []byte, contentType string) error {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	if contentType == "application/trickle-ice-sdpfrag" {
		// Handle ICE candidate
		candidate := strings.TrimSpace(string(body))
		if candidate != "" {
			err := rs.ingress.PeerConn.AddICECandidate(webrtc.ICECandidateInit{
				Candidate: candidate,
			})
			if err != nil {
				return fmt.Errorf("Failed to add ICE candidate: %v", err)
			}
		}
	}

	rs.ingress.LastUpdate = time.Now()
	return nil
}

func (rs *RelayServer) updateWHEPSession(sessionID string, update []byte, contentType string) error {
	rs.mu.RLock()
	for _, session := range rs.egress {
		if session.ID == sessionID {
			if contentType == "application/trickle-ice-sdpfrag" {
				// Handle ICE candidate
				candidate := strings.TrimSpace(string(update))
				if candidate != "" {
					err := session.PeerConn.AddICECandidate(webrtc.ICECandidateInit{
						Candidate: candidate,
					})
					if err != nil {
						return fmt.Errorf("Failed to add ICE candidate: %v", err)
					}
				}
			}

			session.LastUpdate = time.Now()
		}
	}

	rs.mu.RUnlock()
	return nil
}

func (rs *RelayServer) deleteWHIPSession(sessionID string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.cleanupWHIPSession()

	return nil
}

func (rs *RelayServer) deleteWHEPSession(sessionID string) error {
	rs.cleanupWHEPSession(sessionID)
	return nil
}

func (rs *RelayServer) cleanupWHIPSession() {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.ingress.PeerConn != nil {
		rs.ingress.PeerConn.Close()
	}

	clog.Infof(rs.ctx, "Cleaned up WHIP session: %s", rs.ingress.ID)
	rs.ingress = &WHIPSession{} // Reset the session

	if len(rs.egress) == 0 {
		clog.Infof(rs.ctx, "All sessions cleaned up, stopping relay server")
		rs.ctx.Done()
	}
}

func (rs *RelayServer) cleanupWHEPSessions() {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	for i := len(rs.egress) - 1; i >= 0; i-- {
		session := rs.egress[i]
		session.mu.Lock()
		if session.PeerConn != nil {
			session.PeerConn.Close()
		}
		clog.Infof(rs.ctx, "Cleaned up WHEP session: %s", session.ID)
		session.mu.Unlock()
		rs.egress = append(rs.egress[:i], rs.egress[i+1:]...)
	}

	if len(rs.egress) == 0 && rs.ingress.ID == "" {
		clog.Infof(rs.ctx, "All sessions cleaned up, stopping relay server")
		rs.ctx.Done()
	}

}

func (rs *RelayServer) cleanupWHEPSession(sessionID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var sessions []*WHEPSession
	for i, session := range rs.egress {
		if session.ID == sessionID {
			if session.PeerConn != nil {
				session.PeerConn.Close()
			}
			rs.egress = append(rs.egress[:i], rs.egress[i+1:]...)
			clog.Infof(rs.ctx, "Cleaned up WHEP session: %s", sessionID)
			break
		} else {
			sessions = append(sessions, session)
		}
	}
	rs.egress = sessions // Update the egress sessions list

	if len(rs.egress) == 0 && rs.ingress.ID == "" {
		clog.Infof(rs.ctx, "All sessions cleaned up, stopping relay server")
		rs.ctx.Done()
	}
}

func (rs *RelayServer) relayAndTransformTrack(sessionID string, track *webrtc.TrackRemote, sessionType string) {
	clog.Infof(rs.ctx, "Starting relay and transform for track %s from %s session %s", track.Kind().String(), sessionType, sessionID)

	trackKind := track.Kind().String()

	// Read and process RTP packets
	for {
		rtp, _, err := track.ReadRTP()
		//clog.Infof(rs.ctx, "Reading RTP packet for track %s from %s session %s", trackKind, sessionType, sessionID)
		if err != nil {
			if err == io.EOF {
				clog.Infof(rs.ctx, "Track %s from %s session %s ended", trackKind, sessionType, sessionID)
				return
			}
			clog.Infof(rs.ctx, "Error reading RTP: %v", err)
			continue
		}

		// Transform the payload
		if transform, exists := rs.transforms[trackKind]; exists {
			rtp.Payload = transform(rtp.Payload)
		}

		// Update stats
		rs.stats.PacketsReceived++
		rs.stats.BytesReceived += int64(len(rtp.Payload))

		// Relay to all WHEP sessions
		rs.relayToWHEPSessions(rtp, trackKind)
	}
}

// Health check endpoint
func (rs *RelayServer) StreamStats(streamID string) interface{} {
	whepCount := len(rs.egress)
	whipCount := 0
	if rs.ingress != nil && rs.ingress.PeerConn != nil {
		whipCount = 1
	}
	stats := map[string]interface{}{
		"status":              "healthy",
		"whip_sessions":       whipCount,
		"whep_sessions":       whepCount,
		"packets_received":    rs.stats.PacketsReceived,
		"packets_transmitted": rs.stats.PacketsTransmitted,
		"bytes_received":      rs.stats.BytesReceived,
		"bytes_transmitted":   rs.stats.BytesTransmitted,
	}

	jsonBytes, err := json.Marshal(stats)
	if err != nil {
		clog.Infof(rs.ctx, "Error marshalling stats: %v", err)
		return nil
	}
	return jsonBytes
}

// Session management endpoint
func (rs *RelayServer) StreamStatus(streamID string) interface{} {

	sessions := map[string]interface{}{
		"whip_sessions": make([]map[string]interface{}, 0),
		"whep_sessions": make([]map[string]interface{}, 0),
	}
	// Add WHIP session
	sessions["whip_sessions"] = rs.ingress

	// Add WHEP sessions
	for _, session := range rs.egress {
		sessionInfo := map[string]interface{}{
			"id":          session.ID,
			"created":     session.Created,
			"last_update": session.LastUpdate,
			"tracks":      len(session.LocalTracks), // Updated to use LocalTracks
		}
		sessions["whep_sessions"] = append(sessions["whep_sessions"].([]map[string]interface{}), sessionInfo)
	}

	jsonBytes, err := json.Marshal(sessions)
	if err != nil {
		clog.Infof(rs.ctx, "Error marshalling session data: %v", err)
		return nil
	}

	return jsonBytes
}

func parseSessionID(sessionID string) (string, string) {
	lastUnderscoreIndex := strings.LastIndex(sessionID, "_")
	if lastUnderscoreIndex == -1 {
		// No underscore found, return the original string as streamID and empty string as sessionType
		return sessionID, ""
	}

	streamID := sessionID[:lastUnderscoreIndex]
	sessionType := sessionID[lastUnderscoreIndex+1:]

	return streamID, sessionType
}
