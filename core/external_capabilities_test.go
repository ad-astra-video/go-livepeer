package core

import (
	"encoding/json"
	"math/big"
	"testing"

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
		assert.Contains(t, cap.Key, "runner_")

		// Verify it's in the map
		assert.Contains(t, extCaps.Capabilities, cap.Key)
		assert.Equal(t, cap, extCaps.Capabilities[cap.Key])
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
				"id": "runner-a",
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
				"id": "runner-a",
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
		storedCap := extCaps.Capabilities["runner-a"]
		assert.Equal(t, "http://localhost:9000", storedCap.Url)
		assert.Equal(t, 10, storedCap.Capacity)
		assert.NotNil(t, storedCap.price)
	})

	t.Run("Register multiple runners and sort by order", func(t *testing.T) {
		firstJSON := `{
			"name": "multi-cap",
			"url": "http://10.0.0.1:8000",
			"order": 20,
			"capacity": 1,
			"price_per_unit": 100,
			"price_scaling": 1,
			"currency": "wei"
		}`
		secondJSON := `{
			"name": "multi-cap",
			"id": "runner-b",
			"url": "http://10.0.0.2:8000",
			"order": 10,
			"capacity": 2,
			"price_per_unit": 100,
			"price_scaling": 1,
			"currency": "wei"
		}`

		_, err := extCaps.RegisterCapability(firstJSON)
		require.NoError(t, err)
		_, err = extCaps.RegisterCapability(secondJSON)
		require.NoError(t, err)

		caps := extCaps.GetCapabilitiesByName("multi-cap")
		require.Len(t, caps, 2)
		assert.Equal(t, "runner-b", caps[0].Key)
		assert.Contains(t, caps[1].Key, "runner_")

		reserved, err := extCaps.ReserveCapability("multi-cap")
		require.NoError(t, err)
		assert.Equal(t, "runner-b", reserved.Key)
		assert.Equal(t, int64(2), extCaps.AvailableCapacity("multi-cap"))
		require.NoError(t, extCaps.FreeCapability("runner-b"))
	})

	t.Run("Register without id generates unique runner keys", func(t *testing.T) {
		firstJSON := `{
			"name": "generated-key-cap",
			"url": "http://10.0.0.1:8000",
			"capacity": 1,
			"price_per_unit": 100,
			"price_scaling": 1,
			"currency": "wei"
		}`
		secondJSON := `{
			"name": "generated-key-cap",
			"url": "http://10.0.0.1:8000",
			"capacity": 1,
			"price_per_unit": 100,
			"price_scaling": 1,
			"currency": "wei"
		}`

		firstCap, err := extCaps.RegisterCapability(firstJSON)
		require.NoError(t, err)
		secondCap, err := extCaps.RegisterCapability(secondJSON)
		require.NoError(t, err)

		assert.NotEqual(t, firstCap.Key, secondCap.Key)
		assert.Contains(t, firstCap.Key, "runner_")
		assert.Contains(t, secondCap.Key, "runner_")
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
		assert.Contains(t, extCaps.Capabilities, "localhost:8000")

		// Now remove it
		extCaps.RemoveCapability("to-remove")
		assert.NotContains(t, extCaps.Capabilities, "localhost:8000")
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

func TestExternalCapabilities_MultipleRunnersLifecycle(t *testing.T) {
	extCaps := NewExternalCapabilities()

	firstRunner := `{
		"name": "multi-runner-cap",
		"id": "runner-a",
		"url": "http://10.0.0.1:9000",
		"order": 20,
		"capacity": 1,
		"price_per_unit": 100,
		"price_scaling": 1,
		"currency": "wei"
	}`
	secondRunner := `{
		"name": "multi-runner-cap",
		"id": "runner-b",
		"url": "http://10.0.0.2:9000",
		"order": 10,
		"capacity": 2,
		"price_per_unit": 100,
		"price_scaling": 1,
		"currency": "wei"
	}`

	_, err := extCaps.RegisterCapability(firstRunner)
	require.NoError(t, err)
	_, err = extCaps.RegisterCapability(secondRunner)
	require.NoError(t, err)

	runners := extCaps.GetCapabilitiesByName("multi-runner-cap")
	require.Len(t, runners, 2)
	assert.Equal(t, "runner-b", runners[0].Key)
	assert.Equal(t, "runner-a", runners[1].Key)
	assert.Equal(t, int64(3), extCaps.AvailableCapacity("multi-runner-cap"))

	selected, err := extCaps.ReserveCapability("multi-runner-cap")
	require.NoError(t, err)
	assert.Equal(t, "runner-b", selected.Key)
	assert.Equal(t, int64(2), extCaps.AvailableCapacity("multi-runner-cap"))

	extCaps.RemoveCapability("runner-b")
	runners = extCaps.GetCapabilitiesByName("multi-runner-cap")
	require.Len(t, runners, 1)
	assert.Equal(t, "runner-a", runners[0].Key)
	assert.Equal(t, int64(1), extCaps.AvailableCapacity("multi-runner-cap"))

	extCaps.RemoveCapability("multi-runner-cap")
	assert.Empty(t, extCaps.GetCapabilitiesByName("multi-runner-cap"))
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
