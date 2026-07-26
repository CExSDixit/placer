package player

import (
	"fmt"
	"strconv"
)

func fmtArg(format string, v float64) string { return fmt.Sprintf(format, v) }

func fmtNum(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
