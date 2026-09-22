package util

import (
	"fmt"
	"reflect"
)

// 获取类名
func GetClassName(p interface{}) (string, []string) {
	//神奇的获取类名
	tpy := reflect.TypeOf(p)
	rcvr := reflect.ValueOf(p)
	sname := reflect.Indirect(rcvr).Type().Name()
	methods := []string{}
	for m := 0; m < tpy.NumMethod(); m++ {
		method := tpy.Method(m)
		methods = append(methods, method.Name)
	}
	return sname, methods
}

func formatMapKey(values []reflect.Value) string {
	report := ""
	v := values
	if len(values) > 64 {
		v = values[:64]
	}

	for _, v := range v {
		if v.CanInterface() {
			report += fmt.Sprintf("%v, ", v.Interface())
		} else if v.Kind() == reflect.Ptr {
			e := v.Elem()
			if e.CanInterface() {
				report += fmt.Sprintf("%v, ", e.Interface())
			} else {
				report += fmt.Sprintf("NO SUPPORT, ")
			}
		}
	}

	if len(values) > 64 {
		report += "..."
	}

	return report
}

func formatStruct(s reflect.Value, deep int16) string {
	var report string
	if s.Kind() == reflect.Interface {
		s = s.Elem()
	}
	if s.Kind() == reflect.Ptr {
		s = s.Elem()
	}

	prefix := ""
	for strdeep := deep; strdeep >= 0; strdeep-- {
		prefix += "\t"
	}

	typeOfT := s.Type()
	if s.Kind() == reflect.Struct {
		for i := 0; i < s.NumField(); i++ {
			f := s.Field(i)
			if f.Kind() == reflect.Map {
				report += fmt.Sprintf("%s%s keys: {%v}\n", prefix,
					typeOfT.Field(i).Name, formatMapKey(f.MapKeys()))
			} else if (f.Kind() == reflect.Slice) || (f.Kind() == reflect.Array) {
				report += fmt.Sprintf("%s%s len: %d\n", prefix,
					typeOfT.Field(i).Name, f.Len())
			} else if f.Kind() == reflect.Struct {
				if deep > 1 {
					report += fmt.Sprintf("%s%s=%v\n", prefix,
						typeOfT.Field(i).Name, f.Interface())
				} else {
					report += fmt.Sprintf("%s%s:\n", prefix, typeOfT.Field(i).Name)
					report += formatStruct(f, deep+1)
				}
			} else if f.Kind() == reflect.Interface {
				if deep > 1 {
					report += fmt.Sprintf("%s%s=%v\n", prefix,
						typeOfT.Field(i).Name, f.Interface())
				} else {
					report += fmt.Sprintf("%s%s:\n", prefix, typeOfT.Field(i).Name)
					report += formatStruct(f, deep+1)
				}
			} else if f.CanInterface() {
				report += fmt.Sprintf("%s%s=%v\n", prefix,
					typeOfT.Field(i).Name, f.Interface())
			} else if f.Kind() == reflect.Ptr {
				e := f.Elem()
				if f.CanInterface() {
					report += fmt.Sprintf("%s%s=%v\n", prefix,
						typeOfT.Field(i).Name, e.Interface())
				} else {
					report += fmt.Sprintf("%s%s=NO SUPPORT\n", prefix,
						typeOfT.Field(i).Name)
				}
			}
		}
	} else {
		report += fmt.Sprintf("%s%s=%v\n", prefix,
			typeOfT.Name(), s.Interface())
	}
	return report
}

func FormatStruct(obj interface{}) string {
	return formatStruct(reflect.ValueOf(obj), 0)
}
