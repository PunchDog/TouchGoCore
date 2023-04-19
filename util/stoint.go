package util

import "strconv"

func Sto8(v string) int8 {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return int8(n)
}

func Sto16(v string) int16 {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return int16(n)
}

func Sto32(v string) int32 {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return int32(n)
}
func Sto64(v string) int64 {
	val, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return val
}

func Stoi(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

func Stou(v string) uint {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return uint(n)
}
func Stou64(v string) uint64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return uint64(n)
}

func Stobool(v string) bool {
	if v == "0" {
		return false
	} else {
		return true
	}
}

func Stof32(v string) float32 {
	if val, err := strconv.ParseFloat(v, 32); err == nil {
		return float32(val)
	}
	return 0
}

func Stof64(v string) float64 {
	if val, err := strconv.ParseFloat(v, 64); err == nil {
		return val
	}
	return 0
}
