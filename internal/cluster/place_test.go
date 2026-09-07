package cluster

import "testing"

// groups maps the corpus to the three obvious categories, which is what a plan
// written yesterday would hold.
func groupOf(i int) int {
	switch {
	case i < 5:
		return 0
	case i < 10:
		return 1
	case i < 15:
		return 2
	}
	return -1
}

// An incremental run has to file the newcomers and leave everything else
// exactly where the reviewed plan put it.
func TestPlaceFilesNewcomersAndMovesNothingElse(t *testing.T) {
	_, sp := build(t)
	// One member of each group is held back as though it were starred today,
	// along with the two repositories that belong to no category at all.
	newcomers := map[int]bool{4: true, 9: true, 14: true, 15: true, 16: true}

	assign := make([]int, len(sp.Rows))
	for i := range assign {
		if newcomers[i] {
			assign[i] = -1
			continue
		}
		assign[i] = groupOf(i)
	}
	before := append([]int(nil), assign...)

	var todo []int
	for i := range assign {
		if newcomers[i] {
			todo = append(todo, i)
		}
	}
	res := FromMembership(sp, assign, nil, 3)
	placed := Place(sp, res, todo, PlaceOptions{MinSimilarity: 0.05, OutlierSigmas: 3})

	for _, i := range []int{4, 9, 14} {
		if got, want := res.Assign[i], groupOf(i); got != want {
			t.Errorf("corpus[%d] (%s) went to category %d, want %d", i, corpus[i].name, got, want)
		}
	}
	// The cooking notes and the birdwatching notes resemble nothing here, and
	// a category with those in it stops being useful.
	for _, i := range []int{15, 16} {
		if res.Assign[i] != -1 {
			t.Errorf("corpus[%d] (%s) was filed into category %d; it fits none", i, corpus[i].name, res.Assign[i])
		}
	}
	if placed != 3 {
		t.Errorf("placed %d, want 3", placed)
	}
	for i := range before {
		if !newcomers[i] && res.Assign[i] != before[i] {
			t.Errorf("corpus[%d] moved from category %d to %d; an incremental run must not reshuffle",
				i, before[i], res.Assign[i])
		}
	}
}

// A newcomer may join a second list on the same terms as everybody else, and
// the cap has to hold.
func TestPlaceRespectsTheMultiListCap(t *testing.T) {
	_, sp := build(t)
	assign := make([]int, len(sp.Rows))
	for i := range assign {
		assign[i] = groupOf(i)
	}
	assign[4] = -1

	res := FromMembership(sp, assign, nil, 3)
	Place(sp, res, []int{4}, PlaceOptions{
		MinSimilarity: 0.05, OutlierSigmas: 3, MultiLists: 2, MultiRatio: 0.01,
	})
	if res.Assign[4] != 0 {
		t.Fatalf("corpus[4] went to category %d, want 0", res.Assign[4])
	}
	if len(res.Also[4]) > 1 {
		t.Errorf("--multi-list 2 gave %d extra memberships: %v", len(res.Also[4]), res.Also[4])
	}
	for _, c := range res.Also[4] {
		if c == res.Assign[4] {
			t.Error("a secondary membership repeats the primary one")
		}
	}
}

// A plan records the repositories in a category, not which category claimed
// them first, so a centroid has to be built from every member — including the
// ones that are there as a second choice.
func TestFromMembershipCountsSecondaryMembers(t *testing.T) {
	_, sp := build(t)
	assign := make([]int, len(sp.Rows))
	for i := range assign {
		assign[i] = groupOf(i)
	}
	also := make([][]int, len(sp.Rows))
	also[0] = []int{1} // a kubernetes tool listed under the python category too

	plain := FromMembership(sp, append([]int(nil), assign...), nil, 3)
	withAlso := FromMembership(sp, append([]int(nil), assign...), also, 3)

	same := true
	for j := range plain.Centroids[1] {
		if plain.Centroids[1][j] != withAlso.Centroids[1][j] {
			same = false
			break
		}
	}
	if same {
		t.Error("the secondary membership did not reach the centroid")
	}
	for j := range plain.Centroids[2] {
		if plain.Centroids[2][j] != withAlso.Centroids[2][j] {
			t.Error("a category nobody was added to changed anyway")
			break
		}
	}
}
