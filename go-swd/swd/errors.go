package swd

import "errors"

var (
	// ErrNoFactory 没有提供工厂实例
	ErrNoFactory = errors.New("no factory provided")
	// ErrNoDetector 没有提供检测器
	ErrNoDetector = errors.New("no detector provided")
	// ErrNoFilter 没有提供过滤器
	ErrNoFilter = errors.New("no filter provided")
	// ErrNoLoader 没有提供加载器
	ErrNoLoader = errors.New("no loader provided")
	// ErrNoNormalizer 没有提供文本标准化处理器
	ErrNoNormalizer = errors.New("no normalizer provided")
)
