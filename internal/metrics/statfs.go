package metrics

import "errors"

var errStatfsTimeout = errors.New("statfs did not respond within the allotted time")
