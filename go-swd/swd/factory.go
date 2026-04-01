package swd

import (
	"context"
	"fmt"

	"touchgocore/go-swd/config"
	"touchgocore/go-swd/core"
	"touchgocore/go-swd/detector"
	"touchgocore/go-swd/dictionary"
	"touchgocore/go-swd/filter"
)

// DefaultFactory 默认组件工厂实现
type DefaultFactory struct {
	mappingLoader *dictionary.MappingLoader
}

// NewDefaultFactory 创建默认工厂实例
func NewDefaultFactory() ComponentFactory {
	return &DefaultFactory{
		mappingLoader: dictionary.NewMappingLoader(""),
	}
}

// NewDefaultFactoryWithMappingDir 使用指定映射目录创建工厂
func NewDefaultFactoryWithMappingDir(mappingDir string) ComponentFactory {
	return &DefaultFactory{
		mappingLoader: dictionary.NewMappingLoader(mappingDir),
	}
}

// CreateDetector 创建检测器实例
func (f *DefaultFactory) CreateDetector(options *core.SWDOptions) core.Detector {
	detector, err := detector.NewDetector(options)
	if err != nil {
		panic(fmt.Sprintf("创建检测器失败: %v", err))
	}
	return detector
}

// CreateDetectorWithConfig 使用自定义配置创建检测器
func (f *DefaultFactory) CreateDetectorWithConfig(options *core.SWDOptions, cfg *config.MappingConfig) core.Detector {
	// 如果没有提供配置，从文件加载
	if cfg == nil {
		if err := f.mappingLoader.LoadFromFiles(); err != nil {
			panic(fmt.Sprintf("加载映射文件失败: %v", err))
		}
		cfg = f.mappingLoader.GetConfig()
	}

	detector, err := detector.NewDetectorWithConfig(options, cfg)
	if err != nil {
		panic(fmt.Sprintf("创建检测器失败: %v", err))
	}
	return detector
}

// CreateFilter 创建过滤器实例
func (f *DefaultFactory) CreateFilter(detector core.Detector) core.Filter {
	return filter.NewFilter(detector)
}

// CreateLoader 创建加载器实例
func (f *DefaultFactory) CreateLoader() core.Loader {
	loader := dictionary.NewLoader()
	return loader
}

// CreateComponents 创建并关联所有组件
func (f *DefaultFactory) CreateComponents(options *core.SWDOptions) (core.Detector, core.Filter, core.Loader) {
	return f.CreateComponentsWithConfig(options, config.GetGlobalMapping())
}

// CreateComponentsWithConfig 使用自定义配置创建并关联所有组件
func (f *DefaultFactory) CreateComponentsWithConfig(options *core.SWDOptions, cfg *config.MappingConfig) (core.Detector, core.Filter, core.Loader) {
	// 创建加载器
	loader := f.CreateLoader()

	// 加载默认词库
	if err := loader.LoadDefaultWords(context.Background()); err != nil {
		panic(fmt.Sprintf("加载默认词库失败: %v", err))
	}

	// 创建检测器
	d := f.CreateDetectorWithConfig(options, cfg)

	// 注册检测器为加载器的观察者
	if observer, ok := d.(core.Observer); ok {
		loader.(*dictionary.Loader).AddObserver(observer)
	}

	// 创建过滤器
	filter := f.CreateFilter(d)

	return d, filter, loader
}
