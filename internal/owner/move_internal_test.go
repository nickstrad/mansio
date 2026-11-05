package owner

import "testing"

func TestNextMove(t *testing.T) {
	for _, tc := range []struct {
		p    Place
		want moveAction
	}{
		{Place{Owner: "a", Epoch: 1, State: PlaceActive}, moveBegin},
		{Place{Owner: "b", Epoch: 1, State: PlaceActive}, moveClaim},
		{Place{Owner: "a", Epoch: 1, State: PlaceMoving}, moveFenceAndFlip},
		{Place{Owner: "b", Epoch: 1, State: PlaceMoving}, moveFenceAndFlip},
	} {
		if got := nextMove(tc.p, "b"); got != tc.want {
			t.Errorf("nextMove(%+v, b) = %d, want %d", tc.p, got, tc.want)
		}
	}
}
