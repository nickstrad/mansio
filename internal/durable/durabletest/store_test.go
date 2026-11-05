package durabletest_test

import (
	"testing"

	"github.com/nickstrad/ts-db-go/internal/durable"
	"github.com/nickstrad/ts-db-go/internal/durable/durabletest"
)

// The fake must honour the same contract as the S3 adapter, or unit tests against it prove nothing.
func TestFakeStoreContract(t *testing.T) {
	durabletest.RunStoreSuite(t, func(t *testing.T) durable.Store { return durabletest.NewStore() })
}
