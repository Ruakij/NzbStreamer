package diskvGocacheWrapper

import (
	"context"
	"errors"
	"io"
	"reflect"

	"github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/store"
	"github.com/peterbourgon/diskv/v3"
)

type DiskvCacheType interface {
	~[]byte | *io.ReadCloser
}

type DiskvCacheWrapper[T any] struct {
	store *diskv.Diskv
}

func NewDiskvCache[T DiskvCacheType](options diskv.Options) (cache.CacheInterface[T], error) {
	var zeroValue T
	switch reflect.TypeOf(zeroValue) {
	case reflect.TypeOf([]byte(nil)):
	case reflect.TypeOf((*io.ReadCloser)(nil)).Elem():
	default:
		return nil, errors.New("unsupported type for DiskvCacheWrapper")
	}

	return &DiskvCacheWrapper[T]{
		store: diskv.New(options),
	}, nil
}

func (w *DiskvCacheWrapper[T]) Get(ctx context.Context, key any) (T, error) {
	var zeroValue T

	switch key.(type) {
	case string:
	default:
		return zeroValue, errors.New("unsupported key type for DiskvCacheWrapper")
	}

	switch reflect.TypeOf(zeroValue) {
	case reflect.TypeOf([]byte(nil)):
		if data, err := w.store.Read(key.(string)); err != nil {
			return zeroValue, err
		} else {
			return any(data).(T), nil
		}
	case reflect.TypeOf((*io.ReadCloser)(nil)).Elem():
		if reader, err := w.store.ReadStream(key.(string), false); err != nil {
			return zeroValue, err
		} else {
			return any(reader).(T), nil
		}
	default:
		return zeroValue, errors.New("unsupported type for DiskvCacheWrapper")
	}
}

func (w *DiskvCacheWrapper[T]) Set(ctx context.Context, key any, object T, options ...store.Option) error {
	switch key := key.(type) {
	case string:
		valueType := reflect.TypeOf(object)

		if valueType == reflect.TypeOf([]byte(nil)) {
			// Assert object to []byte type safely
			return w.store.Write(key, reflect.ValueOf(object).Bytes())
		} else if valueType.Implements(reflect.TypeOf((*io.Reader)(nil)).Elem()) {
			// Assert object as io.Reader; using .Interface for reflect value, followed by type assertion
			reader, ok := reflect.ValueOf(object).Interface().(io.Reader)
			if !ok {
				return errors.New("failed to cast object to io.Reader")
			}
			return w.store.WriteStream(key, reader, true)
		} else {
			return errors.New("unsupported object type for DiskvCacheWrapper")
		}
	default:
		return errors.New("unsupported key type for DiskvCacheWrapper")
	}
}

func (w *DiskvCacheWrapper[T]) Delete(ctx context.Context, key any) error {
	switch key := key.(type) {
	case string:
		return w.store.Erase(key)
	default:
		return errors.New("unsupported key type for DiskvCacheWrapper")
	}
}

func (w *DiskvCacheWrapper[T]) Invalidate(ctx context.Context, options ...store.InvalidateOption) error {
	// Assuming Invalidate removes certain keys based on options,
	// but since options are not specified, using Clear for now.
	return w.Clear(ctx)
}

func (w *DiskvCacheWrapper[T]) Clear(ctx context.Context) error {
	return w.store.EraseAll()
}

func (w *DiskvCacheWrapper[T]) GetType() string {
	var zeroValue T
	if reflect.TypeOf(zeroValue) == reflect.TypeOf([]byte(nil)) {
		return "[]byte"
	} else if reflect.TypeOf(zeroValue) == reflect.TypeOf((*io.ReadCloser)(nil)).Elem() {
		return "io.ReadCloser"
	}
	return "unknown"
}
