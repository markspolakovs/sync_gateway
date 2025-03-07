package db

import (
	"testing"

	"github.com/couchbase/sync_gateway/base"
	"github.com/stretchr/testify/require"
)

func TestParseScopeCollection(t *testing.T) {
	testCases := []struct {
		collectionString string
		scope            *string
		collection       *string
		err              bool
	}{
		{
			collectionString: "foo.bar",
			scope:            base.StringPtr("foo"),
			collection:       base.StringPtr("bar"),
			err:              false,
		},
		{
			collectionString: "foo",
			scope:            base.StringPtr(base.DefaultScope),
			collection:       base.StringPtr("foo"),
			err:              false,
		},
		{
			collectionString: "",
			scope:            nil,
			collection:       nil,
			err:              true,
		},
		{
			collectionString: ".",
			scope:            nil,
			collection:       nil,
			err:              true,
		},
		{
			collectionString: ".bar",
			scope:            nil,
			collection:       nil,
			err:              true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.collectionString, func(t *testing.T) {
			scope, collection, err := parseScopeAndCollection(testCase.collectionString)
			if testCase.err {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, testCase.scope, scope)
			require.Equal(t, testCase.collection, collection)

		})
	}
}
