package syncmap

type Set struct {
	maps Map
}

func (this *Set) Insert(k interface{}) {
	this.maps.Store(k, true)
}

func (this *Set) IsSet(k interface{}) bool {
	if _, ok := this.maps.Load(k); ok {
		return true
	}
	return false
}

func (this *Set) Foreach(fn func(k interface{})) {
	this.maps.Range(func(k, v interface{}) bool {
		fn(k)
		return true
	})
}
