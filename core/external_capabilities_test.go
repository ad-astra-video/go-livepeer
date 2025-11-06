package core

import (
	"encoding/json"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/livepeer/go-livepeer/eth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestNewExternalCapabilities(t *testing.T) {
	extCaps := NewExternalCapabilities()
	assert.NotNil(t, extCaps)
	assert.NotNil(t, extCaps.Capabilities)
	assert.Empty(t, extCaps.Capabilities)
}

func TestExternalCapabilities_RegisterCapability(t *testing.T) {
	extCaps := NewExternalCapabilities()

	t.Run("Register valid capability", func(t *testing.T) {
		capJSON := `{
			"name": "test-cap",
			"description": "Test capability",
			"url": "http://localhost:8000",
			"capacity": 5,
			"price_per_unit": 100,
			"price_scaling": 1000,
			"currency": "wei"
		}`

		cap, err := extCaps.RegisterCapability(capJSON)
		require.NoError(t, err)
		require.NotNil(t, cap)

		// Verify the capability is stored correctly
		assert.Equal(t, "test-cap", cap.Name)
		assert.Equal(t, "Test capability", cap.Description)
		assert.Equal(t, "http://localhost:8000", cap.Url)
		assert.Equal(t, 5, cap.Capacity)
		assert.Equal(t, int64(100), cap.PricePerUnit)
		assert.Equal(t, int64(1000), cap.PriceScaling)
		assert.Equal(t, "wei", cap.PriceCurrency)
		assert.NotNil(t, cap.price)

		// Verify it's in the map
		assert.Contains(t, extCaps.Capabilities, "test-cap")
		assert.Equal(t, cap, extCaps.Capabilities["test-cap"])
	})

	t.Run("Register with missing price_scaling", func(t *testing.T) {
		capJSON := `{
			"name": "no-scaling",
			"description": "Missing price scaling",
			"url": "http://localhost:8000",
			"capacity": 5,
			"price_per_unit": 100,
			"currency": "wei"
		}`

		cap, err := extCaps.RegisterCapability(capJSON)
		require.NoError(t, err)
		require.NotNil(t, cap)

		// Verify default price_scaling is set to 1
		assert.Equal(t, int64(1), cap.PriceScaling)
	})

	t.Run("Register with invalid JSON", func(t *testing.T) {
		capJSON := `{ invalid json }`

		cap, err := extCaps.RegisterCapability(capJSON)
		assert.Error(t, err)
		assert.Nil(t, cap)
	})

	t.Run("Update existing capability", func(t *testing.T) {
		// First register a capability
		capJSON := `{
			"name": "update-test",
			"description": "Original description",
			"url": "http://localhost:8000",
			"capacity": 5,
			"price_per_unit": 100,
			"price_scaling": 1000,
			"currency": "wei"
		}`

		_, err := extCaps.RegisterCapability(capJSON)
		require.NoError(t, err)

		// Now update it
		updatedJSON := `{
			"name": "update-test",
			"description": "Updated description",
			"url": "http://localhost:9000",
			"capacity": 10,
			"price_per_unit": 200,
			"price_scaling": 2000,
			"currency": "wei"
		}`

		updatedCap, err := extCaps.RegisterCapability(updatedJSON)
		require.NoError(t, err)

		// Check the capability was updated
		assert.Equal(t, "update-test", updatedCap.Name)
		assert.Equal(t, "Updated description", updatedCap.Description)
		assert.Equal(t, "http://localhost:9000", updatedCap.Url)
		assert.Equal(t, 10, updatedCap.Capacity)
		assert.Equal(t, int64(200), updatedCap.PricePerUnit)
		assert.Equal(t, int64(2000), updatedCap.PriceScaling)

		// Verify it's in the map
		storedCap := extCaps.Capabilities["update-test"]
		assert.Equal(t, "http://localhost:9000", storedCap.Url)
		assert.Equal(t, 10, storedCap.Capacity)
		assert.NotNil(t, storedCap.price)
	})
}

func TestExternalCapabilities_RemoveCapability(t *testing.T) {
	extCaps := NewExternalCapabilities()

	t.Run("Remove existing capability", func(t *testing.T) {
		// First register a capability
		capJSON := `{
			"name": "to-remove",
			"description": "Will be removed",
			"url": "http://localhost:8000",
			"capacity": 5,
			"price_per_unit": 100,
			"price_scaling": 1000,
			"currency": "wei"
		}`

		_, err := extCaps.RegisterCapability(capJSON)
		require.NoError(t, err)
		assert.Contains(t, extCaps.Capabilities, "to-remove")

		// Now remove it
		extCaps.RemoveCapability("to-remove")
		assert.NotContains(t, extCaps.Capabilities, "to-remove")
	})

	t.Run("Remove non-existent capability", func(t *testing.T) {
		// Should not panic
		extCaps.RemoveCapability("non-existent")
		// Just verify the map is unchanged
		assert.Equal(t, len(extCaps.Capabilities), 0)
	})

	t.Run("Remove from nil capabilities map", func(t *testing.T) {
		// Create capabilities with nil map
		brokenCaps := &ExternalCapabilities{}
		assert.Nil(t, brokenCaps.Capabilities)

		// Should not panic
		brokenCaps.RemoveCapability("anything")
	})
}

func TestExternalCapability_GetPrice(t *testing.T) {
	extCaps := NewExternalCapabilities()

	t.Run("Get price for valid capability", func(t *testing.T) {
		capJSON := `{
			"name": "price-test",
			"description": "Price test",
			"url": "http://localhost:8000",
			"capacity": 5,
			"price_per_unit": 100,
			"price_scaling": 1000,
			"currency": "wei"
		}`

		cap, err := extCaps.RegisterCapability(capJSON)
		require.NoError(t, err)

		price := cap.GetPrice()
		assert.NotNil(t, price)

		// Verify the price is calculated correctly: price_per_unit / price_scaling = 100/1000 = 0.1
		expected := big.NewRat(100, 1000)
		assert.Equal(t, expected.String(), price.String())
	})

	t.Run("Price conversion with different currencies", func(t *testing.T) {
		currencies := []string{"wei", "eth", "usd"}
		watcherMock := NewPriceFeedWatcherMock(t)
		PriceFeedWatcher = watcherMock
		watcherMock.On("Currencies").Return("ETH", "USD", nil)
		watcherMock.On("Current").Return(eth.PriceData{Price: big.NewRat(100, 1)}, nil)
		watcherMock.On("Subscribe", mock.Anything, mock.Anything).Once()

		for _, currency := range currencies {
			capJSON := `{
				"name": "currency-test",
				"description": "Currency test",
				"url": "http://localhost:8000",
				"capacity": 5,
				"price_per_unit": 100,
				"price_scaling": 1000,
				"currency": "` + currency + `"
			}`

			cap, err := extCaps.RegisterCapability(capJSON)
			if currency == "unknown" {
				assert.Error(t, err)
				continue
			}

			require.NoError(t, err)
			price := cap.GetPrice()
			assert.NotNil(t, price)
		}
	})
}

func TestExternalCapabilities_MarshalJSON(t *testing.T) {
	extCaps := NewExternalCapabilities()

	capJSON := `{
		"name": "json-test",
		"description": "JSON test",
		"url": "http://localhost:8000",
		"capacity": 5,
		"price_per_unit": 100,
		"price_scaling": 1000,
		"currency": "wei"
	}`

	cap, err := extCaps.RegisterCapability(capJSON)
	require.NoError(t, err)

	// Convert the ExternalCapability to JSON
	jsonData, err := json.Marshal(cap)
	require.NoError(t, err)

	// Parse it back
	var parsedCap ExternalCapability
	err = json.Unmarshal(jsonData, &parsedCap)
	require.NoError(t, err)

	// Verify fields were marshalled correctly
	assert.Equal(t, cap.Name, parsedCap.Name)
	assert.Equal(t, cap.Description, parsedCap.Description)
	assert.Equal(t, cap.Url, parsedCap.Url)
	assert.Equal(t, cap.Capacity, parsedCap.Capacity)
	assert.Equal(t, cap.PricePerUnit, parsedCap.PricePerUnit)
	assert.Equal(t, cap.PriceScaling, parsedCap.PriceScaling)
	assert.Equal(t, cap.PriceCurrency, parsedCap.PriceCurrency)

	// Private fields should not be marshalled
	assert.Nil(t, parsedCap.price)
	assert.Equal(t, 0, parsedCap.Load)
}

func TestExternalCapabilities_Concurrency(t *testing.T) {
	extCaps := NewExternalCapabilities()

	// This is a simple test to verify that the locking mechanisms
	// prevent race conditions during concurrent access
	t.Run("Concurrent register and remove", func(t *testing.T) {
		done := make(chan bool)

		// Goroutine to register capabilities
		go func() {
			for i := 0; i < 100; i++ {
				capJSON := `{
					"name": "concurrent-test-` + string(rune('A'+i%26)) + `",
					"description": "Concurrent test",
					"url": "http://localhost:8000",
					"capacity": 5,
					"price_per_unit": 100,
					"price_scaling": 1000,
					"currency": "wei"
				}`

				_, _ = extCaps.RegisterCapability(capJSON)
			}
			done <- true
		}()

		// Goroutine to remove capabilities
		go func() {
			for i := 0; i < 100; i++ {
				extCaps.RemoveCapability("concurrent-test-" + string(rune('A'+i%26)))
			}
			done <- true
		}()

		// Wait for both goroutines to finish
		<-done
		<-done

		// No assertions needed - if there are no race conditions during build with -race flag,
		// then the test passes
	})
}

func TestExternalCapability_ReserveCapacityWithTimeout(t *testing.T) {
	extCaps := NewExternalCapabilities()
	balances := NewAddressBalances(5 * time.Second)
	extCaps.SetBalances(balances)

	capJSON := `{
		"name": "reservation-test",
		"description": "Reservation test",
		"url": "http://localhost:8000",
		"capacity": 5,
		"price_per_unit": 100,
		"price_scaling": 1000,
		"currency": "wei"
	}`

	cap, err := extCaps.RegisterCapability(capJSON)
	require.NoError(t, err)

	t.Run("Reserve available capacity", func(t *testing.T) {
		initialLoad := cap.Load
		reservationID, err := extCaps.ReserveCapacityWithTimeout("reservation-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)
		assert.NotEmpty(t, reservationID)

		// Verify Load was incremented
		assert.Equal(t, initialLoad+1, cap.Load)
		assert.Equal(t, 1, cap.GetReservedCount())
		assert.Equal(t, 4, cap.GetAvailableCapacity("0x0000000000000000000000000000000000000000"))

		// Cleanup
		err = cap.ReleaseReservation(reservationID)
		require.NoError(t, err)
		assert.Equal(t, initialLoad, cap.Load)
	})

	t.Run("Reserve when capacity full", func(t *testing.T) {
		// Reset Load
		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()

		// Reserve all capacity
		var reservationIDs []string
		for i := 0; i < 5; i++ {
			id, err := extCaps.ReserveCapacityWithTimeout("reservation-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
			require.NoError(t, err)
			reservationIDs = append(reservationIDs, id)
		}

		assert.Equal(t, 5, cap.Load)

		// Try to reserve one more - should fail
		_, err := extCaps.ReserveCapacityWithTimeout("reservation-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "capacity unavailable")

		// Cleanup
		for _, id := range reservationIDs {
			cap.ReleaseReservation(id)
		}
		assert.Equal(t, 0, cap.Load)
	})

	t.Run("Reservation expires and Load decrements", func(t *testing.T) {
		// Reset Load
		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()

		// Reserve with short timeout
		_, err := extCaps.ReserveCapacityWithTimeout("reservation-test", 200*time.Millisecond, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)

		assert.Equal(t, 1, cap.Load)
		assert.Equal(t, 1, cap.GetReservedCount())

		// Wait for expiration
		time.Sleep(300 * time.Millisecond)

		// Load should be decremented automatically
		assert.Equal(t, 0, cap.Load)
		assert.Equal(t, 0, cap.GetReservedCount())
		assert.Equal(t, 5, cap.GetAvailableCapacity("0x0000000000000000000000000000000000000000"))
	})

	t.Run("Multiple concurrent reservations", func(t *testing.T) {
		// Reset Load
		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()

		var wg sync.WaitGroup
		var mu sync.Mutex
		successCount := 0

		// Try to reserve capacity concurrently
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := extCaps.ReserveCapacityWithTimeout("reservation-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
				if err == nil {
					mu.Lock()
					successCount++
					mu.Unlock()
				}
			}()
		}

		wg.Wait()

		// Should only have capacity reservations (max 5)
		assert.LessOrEqual(t, successCount, 5)
		assert.Equal(t, successCount, cap.Load)
		assert.Equal(t, successCount, cap.GetReservedCount())

		// Wait for all to expire
		time.Sleep(6 * time.Second)
		assert.Equal(t, 0, cap.Load)
	})
}

func TestExternalCapability_ReleaseReservation(t *testing.T) {
	extCaps := NewExternalCapabilities()
	balances := NewAddressBalances(5 * time.Second)
	extCaps.SetBalances(balances)

	capJSON := `{
		"name": "release-test",
		"description": "Release test",
		"url": "http://localhost:8000",
		"capacity": 5,
		"price_per_unit": 100,
		"price_scaling": 1000,
		"currency": "wei"
	}`

	cap, err := extCaps.RegisterCapability(capJSON)
	require.NoError(t, err)

	t.Run("Release valid reservation", func(t *testing.T) {
		initialLoad := cap.Load
		id, err := extCaps.ReserveCapacityWithTimeout("release-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)

		assert.Equal(t, initialLoad+1, cap.Load)

		err = cap.ReleaseReservation(id)
		assert.NoError(t, err)
		assert.Equal(t, 0, cap.GetReservedCount())
		assert.Equal(t, initialLoad, cap.Load)
	})

	t.Run("Release non-existent reservation", func(t *testing.T) {
		err := cap.ReleaseReservation("non-existent-id")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "reservation not found")
	})

	t.Run("Release already released reservation", func(t *testing.T) {
		id, err := extCaps.ReserveCapacityWithTimeout("release-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)

		// Release once
		err = cap.ReleaseReservation(id)
		assert.NoError(t, err)

		// Try to release again
		err = cap.ReleaseReservation(id)
		assert.Error(t, err)
	})
}

func TestExternalCapability_ExtendReservation(t *testing.T) {
	extCaps := NewExternalCapabilities()
	balances := NewAddressBalances(5 * time.Second)
	extCaps.SetBalances(balances)

	capJSON := `{
		"name": "extend-test",
		"description": "Extend reservation test",
		"url": "http://localhost:8000",
		"capacity": 5,
		"price_per_unit": 100,
		"price_scaling": 1000,
		"currency": "wei"
	}`

	cap, err := extCaps.RegisterCapability(capJSON)
	require.NoError(t, err)

	t.Run("Extend valid reservation", func(t *testing.T) {
		// Reset load
		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()

		// Create a reservation with short duration
		id, err := extCaps.ReserveCapacityWithTimeout("extend-test", 200*time.Millisecond, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)
		assert.Equal(t, 1, cap.Load)

		// Extend it before it expires
		time.Sleep(100 * time.Millisecond)
		err = cap.ExtendReservation(id, 500*time.Millisecond)
		require.NoError(t, err)

		// Wait past the original expiration time
		time.Sleep(200 * time.Millisecond)

		// Should still be reserved (not expired)
		assert.Equal(t, 1, cap.Load)
		assert.Equal(t, 1, cap.GetReservedCount())

		// Wait for the extended expiration
		time.Sleep(400 * time.Millisecond)

		// Now it should be expired
		assert.Equal(t, 0, cap.Load)
		assert.Equal(t, 0, cap.GetReservedCount())
	})

	t.Run("Extend non-existent reservation", func(t *testing.T) {
		err := cap.ExtendReservation("non-existent-id", 5*time.Second)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "reservation not found")
	})

	t.Run("Extend already released reservation", func(t *testing.T) {
		id, err := extCaps.ReserveCapacityWithTimeout("extend-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)

		// Release it
		err = cap.ReleaseReservation(id)
		require.NoError(t, err)

		// Try to extend - should fail
		err = cap.ExtendReservation(id, 5*time.Second)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "reservation not found")
	})

	t.Run("Multiple extensions", func(t *testing.T) {
		// Reset load
		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()

		// Create a reservation
		id, err := extCaps.ReserveCapacityWithTimeout("extend-test", 200*time.Millisecond, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)
		assert.Equal(t, 1, cap.Load)

		// Extend multiple times
		for i := 0; i < 3; i++ {
			time.Sleep(100 * time.Millisecond)
			err = cap.ExtendReservation(id, 300*time.Millisecond)
			require.NoError(t, err, "Extension %d failed", i+1)
			assert.Equal(t, 1, cap.Load)
		}

		// After all extensions, load should still be 1
		assert.Equal(t, 1, cap.Load)
		assert.Equal(t, 1, cap.GetReservedCount())

		// Wait for final expiration
		time.Sleep(400 * time.Millisecond)
		assert.Equal(t, 0, cap.Load)
		assert.Equal(t, 0, cap.GetReservedCount())
	})

	t.Run("Extend updates ExpiresAt time", func(t *testing.T) {
		// Reset load
		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()

		// Create a reservation
		id, err := extCaps.ReserveCapacityWithTimeout("extend-test", 1*time.Second, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)

		// Get the reservation and check initial expiration
		cap.mu.RLock()
		reservation := cap.reservations[id]
		originalExpiresAt := reservation.ExpiresAt
		cap.mu.RUnlock()

		// Wait a bit, then extend
		time.Sleep(200 * time.Millisecond)
		err = cap.ExtendReservation(id, 2*time.Second)
		require.NoError(t, err)

		// Check that ExpiresAt was updated
		cap.mu.RLock()
		newExpiresAt := reservation.ExpiresAt
		cap.mu.RUnlock()

		// New expiration should be after the original
		assert.True(t, newExpiresAt.After(originalExpiresAt),
			"Expected new expiration %v to be after original %v",
			newExpiresAt, originalExpiresAt)

		// Cleanup
		cap.ReleaseReservation(id)
	})

	t.Run("Cannot extend reservation that is already expiring", func(t *testing.T) {
		// Reset load
		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()

		// Create a reservation with very short duration
		id, err := extCaps.ReserveCapacityWithTimeout("extend-test", 50*time.Millisecond, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)

		// Wait for it to expire
		time.Sleep(100 * time.Millisecond)

		// Try to extend - should fail because timer already fired
		err = cap.ExtendReservation(id, 1*time.Second)
		if err != nil {
			// Either "not found" (cleaned up) or "expiring" (timer fired but lock not acquired yet)
			assert.True(t,
				err.Error() == "reservation not found or already expired: "+id ||
					err.Error() == "reservation is expiring or has expired: "+id,
				"Unexpected error: %v", err)
		}
	})
}

func TestExternalCapability_GetAvailableCapacity(t *testing.T) {
	extCaps := NewExternalCapabilities()
	balances := NewAddressBalances(5 * time.Second)
	extCaps.SetBalances(balances)

	capJSON := `{
		"name": "available-test",
		"description": "Available capacity test",
		"url": "http://localhost:8000",
		"capacity": 10,
		"price_per_unit": 100,
		"price_scaling": 1000,
		"currency": "wei"
	}`

	cap, err := extCaps.RegisterCapability(capJSON)
	require.NoError(t, err)

	t.Run("All capacity available", func(t *testing.T) {
		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()

		assert.Equal(t, 10, cap.GetAvailableCapacity("0x0000000000000000000000000000000000000000"))
	})

	t.Run("Capacity with load", func(t *testing.T) {
		cap.mu.Lock()
		cap.Load = 3
		cap.mu.Unlock()

		assert.Equal(t, 7, cap.GetAvailableCapacity("0x0000000000000000000000000000000000000000"))

		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()
	})

	t.Run("Capacity with reservations", func(t *testing.T) {
		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()

		id1, err := extCaps.ReserveCapacityWithTimeout("available-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)
		id2, err := extCaps.ReserveCapacityWithTimeout("available-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)

		// Reservations increment Load
		assert.Equal(t, 2, cap.Load)
		assert.Equal(t, 8, cap.GetAvailableCapacity("0x0000000000000000000000000000000000000000"))

		// Cleanup
		cap.ReleaseReservation(id1)
		cap.ReleaseReservation(id2)
		assert.Equal(t, 0, cap.Load)
	})

	t.Run("No available capacity", func(t *testing.T) {
		cap.mu.Lock()
		cap.Load = 10
		cap.mu.Unlock()

		assert.Equal(t, 0, cap.GetAvailableCapacity("0x0000000000000000000000000000000000000000"))

		cap.mu.Lock()
		cap.Load = 0
		cap.mu.Unlock()
	})
}

func TestExternalCapability_Cleanup(t *testing.T) {
	extCaps := NewExternalCapabilities()
	balances := NewAddressBalances(5 * time.Second)
	extCaps.SetBalances(balances)

	capJSON := `{
		"name": "cleanup-test",
		"description": "Cleanup test",
		"url": "http://localhost:8000",
		"capacity": 5,
		"price_per_unit": 100,
		"price_scaling": 1000,
		"currency": "wei"
	}`

	cap, err := extCaps.RegisterCapability(capJSON)
	require.NoError(t, err)

	t.Run("Cleanup cancels all reservations", func(t *testing.T) {
		// Make multiple reservations
		_, err := extCaps.ReserveCapacityWithTimeout("cleanup-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)
		_, err = extCaps.ReserveCapacityWithTimeout("cleanup-test", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
		require.NoError(t, err)

		assert.Equal(t, 2, cap.GetReservedCount())

		// Cleanup should cancel all timers
		cap.Cleanup()

		// Verify all reservations cleared
		assert.Equal(t, 0, cap.GetReservedCount())

		// Note: Load is not decremented by Cleanup, only timers are stopped
	})
}

func TestExternalCapabilities_RemoveCapabilityWithReservations(t *testing.T) {
	extCaps := NewExternalCapabilities()
	balances := NewAddressBalances(5 * time.Second)
	extCaps.SetBalances(balances)

	capJSON := `{
		"name": "remove-with-reservations",
		"description": "Remove test",
		"url": "http://localhost:8000",
		"capacity": 5,
		"price_per_unit": 100,
		"price_scaling": 1000,
		"currency": "wei"
	}`

	_, err := extCaps.RegisterCapability(capJSON)
	require.NoError(t, err)

	// Make reservations
	_, err = extCaps.ReserveCapacityWithTimeout("remove-with-reservations", 5*time.Second, "0x0000000000000000000000000000000000000000", "")
	require.NoError(t, err)

	// Remove capability should cleanup reservations
	extCaps.RemoveCapability("remove-with-reservations")

	// Verify capability is gone
	assert.NotContains(t, extCaps.Capabilities, "remove-with-reservations")
}
