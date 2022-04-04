package storage_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rancher/opni-monitoring/pkg/core"
	"github.com/rancher/opni-monitoring/pkg/storage"
	"github.com/rancher/opni-monitoring/pkg/test"
)

var _ = Describe("Selection", Label(test.Unit), func() {
	entries := []TableEntry{
		Entry(nil, selector("c1"), cluster("c1"), true),
		Entry(nil, selector("c1"), cluster("c2"), false),
		Entry(nil, selector("c1", "c2"), cluster("c2"), true),
		Entry(nil, selector(), cluster("c1"), true),
		Entry(nil, selector(), cluster("c2"), true),
		Entry(nil, selector(core.MatchOptions_EmptySelectorMatchesNone), cluster("c2"), false),
		Entry(nil, selector(matchLabels("foo", "bar")), cluster("c1"), false),
		Entry(nil, selector(matchLabels("foo", "bar")), cluster("c1", "foo", "baz"), false),
		Entry(nil, selector(matchLabels("foo", "bar")), cluster("c1", "foo", "bar"), true),
		Entry(nil, selector(matchExprs("foo In bar")), cluster("c1", "foo", "bar"), true),
		Entry(nil, selector(matchExprs("foo In baz")), cluster("c1", "foo", "bar"), false),
		Entry(nil, selector(matchExprs("foo In baz,bar")), cluster("c1", "foo", "bar"), true),
		Entry(nil, selector(matchExprs("foo NotIn baz,bar")), cluster("c1", "foo", "bar"), false),
		Entry(nil, selector(matchExprs("foo NotIn baz,bar")), cluster("c1", "foo", "quux"), true),
		Entry(nil, selector(matchExprs("foo NotIn xyz")), cluster("c1", "bar", "baz"), false),
		Entry(nil, selector(matchExprs("foo Exists")), cluster("c1", "foo", "bar"), true),
		Entry(nil, selector(matchExprs("foo Exists")), cluster("c1", "foo", "baz"), true),
		Entry(nil, selector(matchExprs("foo Exists")), cluster("c1", "bar", "baz"), false),
		Entry(nil, selector(matchExprs("foo Exists", "bar Exists")), cluster("c1", "bar", "baz"), false),
		Entry(nil, selector(matchExprs("foo Exists", "bar Exists")), cluster("c1", "bar", "baz", "foo", "quux"), true),
		Entry(nil, selector(matchExprs("foo DoesNotExist")), cluster("c1", "foo", "quux"), false),
		Entry(nil, selector(matchExprs("bar DoesNotExist")), cluster("c1", "foo", "quux"), true),
		Entry(nil, selector(matchExprs("bar DoesNotExist", "foo DoesNotExist")), cluster("c1", "foo", "quux"), false),
		Entry(nil, selector(matchExprs("bar DoesNotExist", "foo DoesNotExist")), cluster("c1", "bar", "quux"), false),
		Entry(nil, selector(matchExprs("bar DoesNotExist", "foo DoesNotExist")), cluster("c1"), true),
		Entry(nil, selector(matchExprs("bar DoesNotExist", "bar Exists")), cluster("c1", "bar", "quux"), false),
		Entry(nil, selector(matchExprs("bar DoesNotExist", "bar Exists")), cluster("c1", "foo", "quux"), false),
	}
	DescribeTable("Label Selector", func(selector storage.ClusterSelector, c *core.Cluster, expected bool) {
		Expect(selector.Predicate()(c)).To(Equal(expected))
	}, entries)
})
