// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package scopedconfig

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/db/storage"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

type NScopedConfig struct {
	coll         string
	AllowedPools []string
	AllowEmpty   bool
	ShallowMerge bool
}

type nScopedConfigEntry struct {
	Pool string `bson:"_id"`
	Val  bson.Raw
}

func FindNScopedConfig(coll string) *NScopedConfig {
	return &NScopedConfig{coll: fmt.Sprintf("scoped_%s", coll)}
}

func (n *NScopedConfig) SetFieldAtomic(pool, name string, value interface{}) (bool, error) {
	coll, err := n.collection()
	if err != nil {
		return false, err
	}
	defer coll.Close()
	name = strings.ToLower(name)
	_, err = coll.Upsert(bson.M{
		"_id": pool,
		"$or": []bson.M{{"val." + name: ""}, {"val." + name: bson.M{"$exists": false}}},
	}, bson.M{"$set": bson.M{"val." + name: value}})
	if err == nil {
		return true, nil
	}
	if mgo.IsDup(err) {
		return false, nil
	}
	return false, err
}

func (n *NScopedConfig) SetField(pool, name string, value interface{}) error {
	coll, err := n.collection()
	if err != nil {
		return err
	}
	defer coll.Close()
	name = strings.ToLower(name)
	_, err = coll.UpsertId(pool, bson.M{"$set": bson.M{"val." + name: value}})
	return err
}

func (n *NScopedConfig) SaveBase(val interface{}) error {
	return n.Save("", val)
}

func (n *NScopedConfig) Save(pool string, val interface{}) error {
	if reflect.TypeOf(val).Kind() != reflect.Struct {
		return errors.New("a struct type is required as value")
	}
	coll, err := n.collection()
	if err != nil {
		return err
	}
	defer coll.Close()
	_, err = coll.UpsertId(pool, bson.M{"_id": pool, "val": val})
	return err
}

func (n *NScopedConfig) SaveMerge(pool string, val interface{}) error {
	newValue := reflect.ValueOf(val)
	if newValue.Type().Kind() != reflect.Struct {
		return errors.New("received object must be a struct")
	}
	coll, err := n.collection()
	if err != nil {
		return err
	}
	defer coll.Close()
	var poolValues nScopedConfigEntry
	previousValue := reflect.New(reflect.ValueOf(val).Type())
	err = coll.FindId(pool).One(&poolValues)
	if err == nil {
		err = poolValues.Val.Unmarshal(previousValue.Interface())
		if err != nil {
			return err
		}
	} else if err != mgo.ErrNotFound {
		return err
	}
	_, err = n.mergeIntoInherited(previousValue.Elem(), reflect.ValueOf(val), false)
	if err != nil {
		return err
	}
	return n.Save(pool, previousValue.Elem().Interface())
}

func (n *NScopedConfig) LoadAll(allVal interface{}) error {
	return n.LoadPools(nil, allVal)
}

func (n *NScopedConfig) LoadPools(filterPools []string, allVal interface{}) error {
	allValValue := reflect.ValueOf(allVal)
	var isPtr bool
	if allValValue.Type().Kind() == reflect.Ptr {
		isPtr = true
		allValValue = allValValue.Elem()
	}
	if allValValue.Type().Kind() != reflect.Map ||
		allValValue.Type().Key().Kind() != reflect.String ||
		allValValue.Type().Elem().Kind() != reflect.Struct {
		return fmt.Errorf("received object must be a map[string]<yourstruct>, received: %v", allValValue.Type())
	}
	if isPtr {
		allValValue.Set(reflect.MakeMap(allValValue.Type()))
	}
	if allValValue.IsNil() {
		return fmt.Errorf("uninitialized map")
	}
	var defaultValues nScopedConfigEntry
	var allPoolValues []nScopedConfigEntry
	coll, err := n.collection()
	if err != nil {
		return err
	}
	defer coll.Close()
	err = coll.FindId("").One(&defaultValues)
	if err != nil && err != mgo.ErrNotFound {
		return err
	}
	mapType := allValValue.Type().Elem()
	baseValue := reflect.New(mapType)
	baseVal := baseValue.Interface()
	if defaultValues.Val.Data != nil {
		err = defaultValues.Val.Unmarshal(baseVal)
		if err != nil {
			return err
		}
	}
	allValValue.SetMapIndex(reflect.ValueOf(""), baseValue.Elem())
	if len(filterPools) == 0 {
		err = coll.Find(bson.M{"_id": bson.M{"$ne": ""}}).All(&allPoolValues)
	} else {
		err = coll.Find(bson.M{"_id": bson.M{"$in": filterPools}}).All(&allPoolValues)
	}
	if err != nil && err != mgo.ErrNotFound {
		return err
	}
	for i := range allPoolValues {
		baseValue = reflect.New(mapType)
		baseVal = baseValue.Interface()
		if defaultValues.Val.Data != nil {
			err = defaultValues.Val.Unmarshal(baseVal)
			if err != nil {
				return err
			}
		}
		poolValue := reflect.New(mapType)
		poolVal := poolValue.Interface()
		err = allPoolValues[i].Val.Unmarshal(poolVal)
		if err != nil {
			return err
		}
		_, err = n.mergeInto(baseValue.Elem(), poolValue.Elem())
		if err != nil {
			return err
		}
		allValValue.SetMapIndex(reflect.ValueOf(allPoolValues[i].Pool), baseValue.Elem())
	}
	return nil
}

func (n *NScopedConfig) Load(pool string, poolVal interface{}) error {
	return n.LoadWithBase(pool, nil, poolVal)
}

func (n *NScopedConfig) LoadBase(poolVal interface{}) error {
	return n.LoadWithBase("", nil, poolVal)
}

func (n *NScopedConfig) LoadWithBase(pool string, baseVal interface{}, poolVal interface{}) error {
	poolValue := reflect.ValueOf(poolVal)
	if poolValue.Type().Kind() != reflect.Ptr ||
		poolValue.Elem().Type().Kind() != reflect.Struct {
		return errors.New("received object must be a pointer to a struct")
	}
	var baseValue reflect.Value
	if baseVal == nil {
		baseValue = reflect.New(poolValue.Elem().Type())
		baseVal = baseValue.Interface()
	} else {
		baseValue = reflect.ValueOf(baseVal)
		if baseValue.Type().Kind() != reflect.Ptr {
			return errors.New("received object must be a pointer to a struct")
		}
		if poolValue.Elem().Type() != baseValue.Elem().Type() {
			return errors.New("received object must the same type")
		}
	}
	coll, err := n.collection()
	if err != nil {
		return err
	}
	defer coll.Close()
	var defaultValues, poolValues nScopedConfigEntry
	err = coll.FindId("").One(&defaultValues)
	if err == nil {
		err = defaultValues.Val.Unmarshal(baseVal)
		if err != nil {
			return err
		}
	} else if err != mgo.ErrNotFound {
		return err
	}
	if pool == "" {
		poolValue.Elem().Set(baseValue.Elem())
		return nil
	}
	baseCopy := reflect.New(baseValue.Elem().Type())
	if defaultValues.Val.Data != nil {
		baseCopyVal := baseCopy.Interface()
		err = defaultValues.Val.Unmarshal(baseCopyVal)
		if err != nil {
			return err
		}
	}
	err = coll.FindId(pool).One(&poolValues)
	if err == nil {
		err = poolValues.Val.Unmarshal(poolVal)
		if err != nil {
			return err
		}
	} else if err != mgo.ErrNotFound {
		return err
	}
	_, err = n.mergeInto(baseCopy.Elem(), poolValue.Elem())
	if err != nil {
		return err
	}
	poolValue.Elem().Set(baseCopy.Elem())
	return nil
}

func (n *NScopedConfig) Remove(pool string) error {
	coll, err := n.collection()
	if err != nil {
		return err
	}
	defer coll.Close()
	return coll.RemoveId(pool)
}

func (n *NScopedConfig) RemoveField(pool, name string) error {
	coll, err := n.collection()
	if err != nil {
		return err
	}
	defer coll.Close()
	name = strings.ToLower(name)
	err = coll.UpdateId(pool, bson.M{"$unset": bson.M{"val." + name: ""}})
	if err != nil && err != mgo.ErrNotFound {
		return err
	}
	return nil
}

func (n *NScopedConfig) mergeInto(base reflect.Value, pool reflect.Value) (merged bool, err error) {
	return n.mergeIntoInherited(base, pool, true)
}

func (n *NScopedConfig) mergeIntoInherited(base reflect.Value, pool reflect.Value, setInherited bool) (merged bool, err error) {
	switch base.Kind() {
	case reflect.Struct:
		if _, isTime := base.Interface().(time.Time); isTime {
			if !n.isEmpty(pool) {
				base.Set(pool)
			}
			break
		}
		numField := base.Type().NumField()
		for i := 0; i < numField; i++ {
			fieldType := base.Type().Field(i)
			isInherited := strings.HasSuffix(strings.ToLower(fieldType.Name), "inherited")
			if isInherited {
				continue
			}
			inheritedField, hasInherited := base.Type().FieldByNameFunc(func(name string) bool {
				return strings.ToLower(name) == strings.ToLower(fieldType.Name+"inherited")
			})
			if fieldType.PkgPath != "" && !fieldType.Anonymous {
				continue
			}
			f1Value := base.Field(i)
			f2Value := pool.Field(i)
			if n.ShallowMerge {
				if !n.isEmpty(f2Value) {
					merged = true
					f1Value.Set(f2Value)
				}
				continue
			}
			var fieldMerged bool
			fieldMerged, err = n.mergeIntoInherited(f1Value, f2Value, setInherited)
			if err != nil {
				return
			}
			if setInherited && hasInherited && inheritedField.Type.Kind() == reflect.Bool {
				base.FieldByIndex(inheritedField.Index).Set(reflect.ValueOf(!fieldMerged))
			}
			if fieldMerged {
				merged = true
			}
		}
	case reflect.Map:
		for _, k := range pool.MapKeys() {
			poolVal := pool.MapIndex(k)
			if !n.isEmpty(poolVal) {
				merged = true
				if base.IsNil() {
					base.Set(reflect.MakeMap(reflect.MapOf(k.Type(), poolVal.Type())))
				}
				base.SetMapIndex(k, poolVal)
			} else {
				base.SetMapIndex(k, reflect.Value{})
			}
		}
	default:
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("error trying to set field: %s", r)
			}
		}()
		if !n.isEmpty(pool) {
			merged = true
			base.Set(pool)
		}
	}
	return
}

func (n *NScopedConfig) isEmpty(valValue reflect.Value) bool {
	switch valValue.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Ptr, reflect.Interface, reflect.Slice:
		if valValue.IsNil() {
			return true
		}
	}
	if n.AllowEmpty {
		return false
	}
	if valValue.Kind() == reflect.Slice && valValue.Len() == 0 {
		return true
	}
	cmpValue := valValue
	zero := reflect.Zero(valValue.Type()).Interface()
	if valValue.Kind() == reflect.Ptr {
		cmpValue = valValue.Elem()
		zero = reflect.Zero(valValue.Elem().Type()).Interface()
	}
	return reflect.DeepEqual(cmpValue.Interface(), zero)
}

func (n *NScopedConfig) collection() (*storage.Collection, error) {
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	return conn.Collection(n.coll), nil
}
