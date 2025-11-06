package core

import (
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"sync"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/golang/glog"
)

// Reservation represents a time-based reservation of capacity
type Reservation struct {
	ID           string
	ExpiresAt    time.Time
	capability   *ExternalCapability
	timer        *time.Timer
	chargeTicker *time.Ticker
	stopCharge   chan struct{}
	sender       ethcommon.Address
	manifestID   ManifestID
}

type ExternalCapability struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	Url           string `json:"url"`
	Capacity      int    `json:"capacity"`
	PricePerUnit  int64  `json:"price_per_unit"`
	PriceScaling  int64  `json:"price_scaling"`
	PriceCurrency string `json:"currency"`

	price *AutoConvertedPrice

	mu           sync.RWMutex
	Load         int
	reservations map[string]*Reservation
}

type ExternalCapabilities struct {
	capm         sync.Mutex
	Capabilities map[string]*ExternalCapability
	balances     *AddressBalances // Shared reference to balances for charging reservations
}

func NewExternalCapabilities() *ExternalCapabilities {
	return &ExternalCapabilities{
		Capabilities: make(map[string]*ExternalCapability),
	}
}

// SetBalances sets the AddressBalances reference for all capabilities
func (extCaps *ExternalCapabilities) SetBalances(balances *AddressBalances) {
	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()
	extCaps.balances = balances
}

func (extCaps *ExternalCapabilities) RemoveCapability(extCap string) {
	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()

	if cap, ok := extCaps.Capabilities[extCap]; ok {
		cap.Cleanup()
	}
	delete(extCaps.Capabilities, extCap)
}

func (extCaps *ExternalCapabilities) RegisterCapability(extCapability string) (*ExternalCapability, error) {
	extCaps.capm.Lock()
	defer extCaps.capm.Unlock()
	if extCaps.Capabilities == nil {
		extCaps.Capabilities = make(map[string]*ExternalCapability)
	}
	var extCap ExternalCapability
	err := json.Unmarshal([]byte(extCapability), &extCap)
	if err != nil {
		return nil, err
	}

	//ensure PriceScaling is not 0
	if extCap.PriceScaling == 0 {
		extCap.PriceScaling = 1
	}
	extCap.price, err = NewAutoConvertedPrice(extCap.PriceCurrency, big.NewRat(extCap.PricePerUnit, extCap.PriceScaling), func(price *big.Rat) {
		glog.V(6).Infof("Capability %s price set to %s wei per compute unit", extCap.Name, price.FloatString(3))
	})

	if err != nil {
		panic(fmt.Errorf("error converting price: %v", err))
	}

	// Initialize or preserve reservations map
	if cap, ok := extCaps.Capabilities[extCap.Name]; ok {
		cap.Url = extCap.Url
		cap.Capacity = extCap.Capacity
		cap.price = extCap.price
		// Keep existing reservations map
		extCap.reservations = cap.reservations
	} else {
		// New capability - create reservations map
		extCap.reservations = make(map[string]*Reservation)
	}

	extCaps.Capabilities[extCap.Name] = &extCap

	return &extCap, err
}

// ReserveCapacityWithTimeout is a wrapper that reserves capacity with timeout on a specific capability
// It passes the shared balances reference from ExternalCapabilities to the capability
func (extCaps *ExternalCapabilities) ReserveCapacityWithTimeout(capabilityName string, duration time.Duration, sender string, requestID string) (string, error) {
	extCaps.capm.Lock()
	cap, ok := extCaps.Capabilities[capabilityName]
	balances := extCaps.balances
	extCaps.capm.Unlock()

	if !ok {
		return "", fmt.Errorf("capability not found: %s", capabilityName)
	}

	return cap.ReserveCapacityWithTimeout(duration, sender, requestID, balances)
}

func (extCap *ExternalCapability) GetPrice() *big.Rat {
	extCap.mu.RLock()
	defer extCap.mu.RUnlock()
	return extCap.price.Value()
}

// createExpirationTimer creates a timer that will decrement Load and remove the reservation when it expires
// This function must be called while holding the extCap.mu lock
func (extCap *ExternalCapability) createExpirationTimer(reservationID string, duration time.Duration) *time.Timer {
	return time.AfterFunc(duration, func() {
		extCap.mu.Lock()
		defer extCap.mu.Unlock()

		// Check if reservation still exists (not manually released)
		if reservation, exists := extCap.reservations[reservationID]; exists {
			// Stop the charging ticker
			if reservation.chargeTicker != nil {
				reservation.chargeTicker.Stop()
			}
			close(reservation.stopCharge)

			extCap.Load--
			delete(extCap.reservations, reservationID)
			glog.V(6).Infof("Reservation expired and Load decremented: %s (load now %d)",
				reservationID, extCap.Load)
		}
	})
}

// periodicCharge charges the sender's balance every 5 seconds for the reserved capacity
func (extCap *ExternalCapability) periodicCharge(reservation *Reservation, balances *AddressBalances) {
	defer func() {
		if reservation.chargeTicker != nil {
			reservation.chargeTicker.Stop()
		}
	}()

	for {
		select {
		case <-reservation.stopCharge:
			glog.V(6).Infof("Stopped periodic charging for reservation %s", reservation.ID)
			return
		case <-reservation.chargeTicker.C:
			// Charge for 5 seconds of reserved capacity
			extCap.chargeReservation(reservation, 5, balances)
		}
	}
}

// chargeReservation debits the balance for the given number of seconds
func (extCap *ExternalCapability) chargeReservation(reservation *Reservation, seconds int64, balances *AddressBalances) {
	if balances == nil {
		glog.V(6).Infof("Skipping charge for reservation %s: no balances available", reservation.ID)
		return
	}

	// Price is per unit (second in this case)
	price := extCap.GetPrice()
	if price == nil {
		glog.V(6).Infof("Skipping charge for reservation %s: no price set", reservation.ID)
		return
	}

	// Calculate fee for the number of seconds
	fee := new(big.Rat).Mul(price, big.NewRat(seconds, 1))

	// Debit the sender's balance
	balances.Debit(reservation.sender, reservation.manifestID, fee)

	glog.V(6).Infof("Charged reservation %s: sender=%s, manifestID=%s, fee=%s for %d seconds",
		reservation.ID, reservation.sender.Hex(), reservation.manifestID, fee.FloatString(3), seconds)
}

// HasReservation checks if a reservation with the given ID exists
func (extCap *ExternalCapability) HasReservation(reservationID string) bool {
	extCap.mu.Lock()
	defer extCap.mu.Unlock()

	_, exists := extCap.reservations[reservationID]
	return exists
}

// ReserveCapacityWithTimeout reserves capacity for a specific duration
// The Load is incremented immediately and will be decremented when the reservation expires
// Returns a reservation ID that can be used to release the reservation early
// Charges the sender's balance for the capability every 5 seconds while the reservation is active
// If requestID is provided, it will be appended to the generated reservation ID
func (extCap *ExternalCapability) ReserveCapacityWithTimeout(duration time.Duration, sender string, requestID string, balances *AddressBalances) (string, error) {
	extCap.mu.Lock()
	defer extCap.mu.Unlock()

	// Check if capacity is available
	if extCap.Load >= extCap.Capacity {
		return "", fmt.Errorf("capacity unavailable: load=%d, capacity=%d",
			extCap.Load, extCap.Capacity)
	}

	//convert sender string to ethcommon.Address
	senderAddr := ethcommon.HexToAddress(sender)

	// Generate reservation ID, appending the request ID if provided
	var reservationID string
	if requestID != "" {
		reservationID = string(RandomManifestID()) + "-" + requestID
	} else {
		reservationID = string(RandomManifestID())
	}

	// Increment Load immediately
	extCap.Load++

	// Create reservation with timer for automatic expiration
	reservation := &Reservation{
		ID:         reservationID,
		ExpiresAt:  time.Now().Add(duration),
		capability: extCap,
		sender:     senderAddr,
		stopCharge: make(chan struct{}),
	}

	// Set up timer to decrement Load when reservation expires
	reservation.timer = extCap.createExpirationTimer(reservationID, duration)

	// Start periodic charging every 5 seconds
	reservation.chargeTicker = time.NewTicker(5 * time.Second)
	go extCap.periodicCharge(reservation, balances)

	extCap.reservations[reservationID] = reservation

	glog.V(6).Infof("Reserved capacity for %s: id=%s, duration=%s, load now %d, sender=%s",
		extCap.Name, reservationID, duration, extCap.Load, senderAddr.Hex())

	return reservationID, nil
}

// ReleaseReservation releases a previously made reservation early
// This decrements Load and cancels the expiration timer
func (extCap *ExternalCapability) ReleaseReservation(reservationID string) error {
	extCap.mu.Lock()
	defer extCap.mu.Unlock()

	reservation, exists := extCap.reservations[reservationID]
	if !exists {
		return fmt.Errorf("reservation not found or already expired: %s", reservationID)
	}

	// Stop the timer
	reservation.timer.Stop()

	// Stop the charging ticker
	if reservation.chargeTicker != nil {
		reservation.chargeTicker.Stop()
	}
	close(reservation.stopCharge)

	// Decrement Load
	extCap.Load--

	// Remove reservation
	delete(extCap.reservations, reservationID)

	glog.V(6).Infof("Released reservation early: %s (load now %d)", reservationID, extCap.Load)
	return nil
}

// ExtendReservation extends an existing reservation by additional duration
// The new expiration time will be: current time + additionalDuration
// Note: This does NOT extend from the original expiration time, but from now
func (extCap *ExternalCapability) ExtendReservation(reservationID string, additionalDuration time.Duration) error {
	extCap.mu.Lock()
	defer extCap.mu.Unlock()

	reservation, exists := extCap.reservations[reservationID]
	if !exists {
		return fmt.Errorf("reservation not found or already expired: %s", reservationID)
	}

	// Stop the existing timer
	if !reservation.timer.Stop() {
		// Timer already fired, reservation is expiring/expired
		return fmt.Errorf("reservation is expiring or has expired: %s", reservationID)
	}

	// Update expiration time
	newExpiresAt := time.Now().Add(additionalDuration)
	reservation.ExpiresAt = newExpiresAt

	// Create new timer for extended duration using the shared helper function
	reservation.timer = extCap.createExpirationTimer(reservationID, additionalDuration)

	glog.V(6).Infof("Extended reservation %s for %s: new expiration=%s",
		reservationID, extCap.Name, newExpiresAt.Format(time.RFC3339))

	return nil
}

// GetAvailableCapacity returns the available capacity
func (extCap *ExternalCapability) GetAvailableCapacity(sender string) int {
	extCap.mu.RLock()
	defer extCap.mu.RUnlock()

	available := extCap.Capacity - extCap.Load
	if available < 0 {
		available = 0
	}
	if available == 0 {
		// Check for existing reservations by the sender
		// this allows for sending new job tokens if the sender has reserved capacity already
		senderAddr := ethcommon.HexToAddress(sender)
		for _, reservation := range extCap.reservations {
			if reservation.sender == senderAddr {
				available += 1
			}
		}
	}

	return available
}

// GetReservedCount returns the number of active reservations
func (extCap *ExternalCapability) GetReservedCount() int {
	extCap.mu.RLock()
	defer extCap.mu.RUnlock()

	return len(extCap.reservations)
}

// Cleanup cancels all active reservations
// Should be called when the capability is being removed
func (extCap *ExternalCapability) Cleanup() {
	extCap.mu.Lock()
	defer extCap.mu.Unlock()

	// Stop all timers and charging tickers
	for _, reservation := range extCap.reservations {
		if reservation.timer != nil {
			reservation.timer.Stop()
		}
		if reservation.chargeTicker != nil {
			reservation.chargeTicker.Stop()
		}
		if reservation.stopCharge != nil {
			close(reservation.stopCharge)
		}
	}

	// Clear reservations
	extCap.reservations = make(map[string]*Reservation)
}
