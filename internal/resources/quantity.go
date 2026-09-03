package resources

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type Usage struct {
	CPUMillicores int64 `json:"cpuMillicores"`
	MemoryBytes   int64 `json:"memoryBytes"`
}

func (u *Usage) Add(other Usage) {
	u.CPUMillicores += other.CPUMillicores
	u.MemoryBytes += other.MemoryBytes
}

func ParseCPU(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	multiplier := float64(1000)
	for suffix, scale := range map[string]float64{
		"n": 0.000001,
		"u": 0.001,
		"m": 1,
	} {
		if strings.HasSuffix(value, suffix) {
			value = strings.TrimSuffix(value, suffix)
			multiplier = scale
			break
		}
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number < 0 {
		return 0, fmt.Errorf("invalid CPU quantity %q", value)
	}
	return int64(math.Round(number * multiplier)), nil
}

func ParseCPUPercent(value string) (int64, error) {
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "%"))
	if value == "" {
		return 0, nil
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number < 0 {
		return 0, fmt.Errorf("invalid CPU percentage %q", value)
	}
	return int64(math.Round(number * 10)), nil
}

func ParseBytes(value string) (int64, error) {
	value = strings.TrimSpace(strings.SplitN(value, "/", 2)[0])
	if value == "" {
		return 0, nil
	}
	unitStart := len(value)
	for unitStart > 0 {
		character := value[unitStart-1]
		if (character >= '0' && character <= '9') || character == '.' {
			break
		}
		unitStart--
	}
	numberText := strings.TrimSpace(value[:unitStart])
	unit := strings.ToLower(strings.TrimSpace(value[unitStart:]))
	number, err := strconv.ParseFloat(numberText, 64)
	if err != nil || number < 0 {
		return 0, fmt.Errorf("invalid memory quantity %q", value)
	}
	multipliers := map[string]float64{
		"":    1,
		"b":   1,
		"k":   1000,
		"kb":  1000,
		"ki":  1 << 10,
		"kib": 1 << 10,
		"m":   1000 * 1000,
		"mb":  1000 * 1000,
		"mi":  1 << 20,
		"mib": 1 << 20,
		"g":   1000 * 1000 * 1000,
		"gb":  1000 * 1000 * 1000,
		"gi":  1 << 30,
		"gib": 1 << 30,
		"t":   1000 * 1000 * 1000 * 1000,
		"tb":  1000 * 1000 * 1000 * 1000,
		"ti":  1 << 40,
		"tib": 1 << 40,
	}
	multiplier, ok := multipliers[unit]
	if !ok {
		return 0, fmt.Errorf("unsupported memory unit %q", unit)
	}
	return int64(math.Round(number * multiplier)), nil
}
