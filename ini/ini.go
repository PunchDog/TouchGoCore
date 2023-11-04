package ini

import (
	"fmt"
	"os"

	"github.com/go-ini/ini"
)

type IniParser struct {
	conf_reader *ini.File // config reader
}

type IniParserError struct {
	error_info string
}

func (e *IniParserError) Error() string { return e.error_info }

func (this *IniParser) Load(config_file_name string) error {
	conf, err := ini.Load(config_file_name)
	if err != nil {
		this.conf_reader = nil
		return err
	}
	this.conf_reader = conf
	return nil
}

func (this *IniParser) GetString(section string, key string, szdefault string) string {
	if this.conf_reader == nil {
		return szdefault
	}

	s := this.conf_reader.Section(section)
	if s == nil {
		return szdefault
	}

	value := s.Key(key).String()
	if value == "" {
		return szdefault
	}

	return value
}

func (this *IniParser) GetInt32(section string, key string, idefault int32) int32 {
	if this.conf_reader == nil {
		return idefault
	}

	s := this.conf_reader.Section(section)
	if s == nil {
		return idefault
	}

	value_int, _ := s.Key(key).Int()

	return int32(value_int)
}

func (this *IniParser) GetUint32(section string, key string, idefault uint32) uint32 {
	if this.conf_reader == nil {
		return idefault
	}

	s := this.conf_reader.Section(section)
	if s == nil {
		return idefault
	}

	value_int, _ := s.Key(key).Uint()

	return uint32(value_int)
}

func (this *IniParser) GetInt64(section string, key string, idefault int64) int64 {
	if this.conf_reader == nil {
		return idefault
	}

	s := this.conf_reader.Section(section)
	if s == nil {
		return idefault
	}

	value_int, _ := s.Key(key).Int64()
	return value_int
}

func (this *IniParser) GetUint64(section string, key string, idefault uint64) uint64 {
	if this.conf_reader == nil {
		return idefault
	}

	s := this.conf_reader.Section(section)
	if s == nil {
		return idefault
	}

	value_int, _ := s.Key(key).Uint64()
	return value_int
}

func (this *IniParser) GetFloat32(section string, key string, fdefault float32) float32 {
	if this.conf_reader == nil {
		return fdefault
	}

	s := this.conf_reader.Section(section)
	if s == nil {
		return fdefault
	}

	value_float, _ := s.Key(key).Float64()
	return float32(value_float)
}

func (this *IniParser) GetFloat64(section string, key string, fdefault float64) float64 {
	if this.conf_reader == nil {
		return fdefault
	}

	s := this.conf_reader.Section(section)
	if s == nil {
		return fdefault
	}

	value_float, _ := s.Key(key).Float64()
	return value_float
}

// 获取所有元素
func (this *IniParser) GetAll(section string) map[string]string {
	if this.conf_reader == nil {
		return nil
	}
	s, err := this.conf_reader.GetSection(section)
	if err != nil {
		return nil
	}

	return s.KeysHash()
}

// 读取配置
func Load(path string) (p *IniParser, err error) {
	ini_parser := &IniParser{}
	if err1 := ini_parser.Load(path); err != nil {
		p = nil
		err = &IniParserError{fmt.Sprintf("try load config file[%s] error[%s]\n", path, err1.Error())}
		return
	}
	p = ini_parser
	err = nil
	return
}

func LoadConfigByNoSectionName(tpath string) map[string]map[string]string {
	cfg, err := ini.Load(tpath)
	if err != nil {
		fmt.Println("Failed to load configuration file: %v", err)
		os.Exit(-1)
	}
	ret := make(map[string]map[string]string)
	sectionNamelist := cfg.SectionStrings()
	for _, sectionName := range sectionNamelist {
		mp, err := cfg.GetSection(sectionName)
		if err == nil {
			ret[sectionName] = mp.KeysHash()
		}
	}
	return ret
}
