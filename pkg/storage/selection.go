package storage

import "github.com/kralicky/opni-monitoring/pkg/core"

type SelectorPredicate func(*core.Cluster) bool

type ClusterSelector struct {
	ClusterIDs    []string
	LabelSelector *core.LabelSelector
}

func (p ClusterSelector) Predicate() SelectorPredicate {
	if p.LabelSelector == nil && len(p.ClusterIDs) == 0 {
		return func(c *core.Cluster) bool { return true }
	}
	idSet := map[string]struct{}{}
	for _, id := range p.ClusterIDs {
		idSet[id] = struct{}{}
	}
	return func(c *core.Cluster) bool {
		id := c.Id
		if _, ok := idSet[id]; ok {
			return true
		}
		if p.LabelSelector == nil {
			return false
		}
		return labelSelectorMatches(p.LabelSelector, c.Labels)
	}
}

func labelSelectorMatches(selector *core.LabelSelector, labels map[string]string) bool {
	for key, value := range selector.MatchLabels {
		if labels[key] != value {
			return false
		}
	}
	for _, req := range selector.MatchExpressions {
		switch core.LabelSelectorOperator(req.Operator) {
		case core.LabelSelectorOpIn:
			ok := false
			for _, value := range req.Values {
				if labels[req.Key] == value {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		case core.LabelSelectorOpNotIn:
			ok := true
			for _, value := range req.Values {
				if labels[req.Key] == value {
					ok = false
					break
				}
			}
			if !ok {
				return false
			}
		case core.LabelSelectorOpExists:
			if _, ok := labels[req.Key]; !ok {
				return false
			}
		case core.LabelSelectorOpDoesNotExist:
			if _, ok := labels[req.Key]; ok {
				return false
			}
		}
	}
	return true
}