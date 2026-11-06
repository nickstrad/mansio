package vactortest_test

import (
	"testing"

	"github.com/nickstrad/mansio/server/internal/vactor"
	"github.com/nickstrad/mansio/server/internal/vactor/vactortest"
)

// The fake must honour the same contract as the real S3 fetcher, or unit tests against it prove nothing.
func TestFakeFetcherContract(t *testing.T) {
	vactortest.RunFetcherSuite(t, func(t *testing.T) (vactor.Fetcher, func(string, []byte)) {
		f := vactortest.NewFetcher()
		return f, f.Put
	})
}
