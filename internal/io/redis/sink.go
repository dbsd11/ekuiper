// Copyright 2021-2024 EMQ Technologies Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package redis

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lf-edge/ekuiper/contract/v2/api"
	"github.com/redis/go-redis/v9"

	"github.com/lf-edge/ekuiper/v2/internal/pkg/util"
	"github.com/lf-edge/ekuiper/v2/pkg/ast"
	"github.com/lf-edge/ekuiper/v2/pkg/cast"
)

type config struct {
	// host:port address.
	Addr     string `json:"addr,omitempty"`
	Username string `json:"username,omitempty"`
	// Optional password. Must match the password specified in the
	Password string `json:"password,omitempty"`
	// Database to be selected after connecting to the server.
	Db int `json:"db,omitempty"`
	// key of field
	Field string `json:"field,omitempty"`
	// key define
	Key          string            `json:"key,omitempty"`
	KeyType      string            `json:"keyType,omitempty"`
	DataType     string            `json:"dataType,omitempty"`
	Expiration   cast.DurationConf `json:"expiration,omitempty"`
	RowkindField string            `json:"rowkindField"`
	DataTemplate string            `json:"dataTemplate"`
	Fields       []string          `json:"fields"`
	DataField    string            `json:"dataField"`
}

type RedisSink struct {
	c   *config
	cli *redis.Client
}

func (r *RedisSink) Provision(_ api.StreamContext, props map[string]any) error {
	return r.Validate(props)
}

func (r *RedisSink) Connect(ctx api.StreamContext, sch api.StatusChangeHandler) error {
	logger := ctx.GetLogger()
	logger.Debug("Opening redis sink")

	r.cli = redis.NewClient(&redis.Options{
		Addr:     r.c.Addr,
		Username: r.c.Username,
		Password: r.c.Password,
		DB:       r.c.Db, // use default DB
	})
	_, err := r.cli.Ping(ctx).Result()
	if err != nil {
		sch(api.ConnectionDisconnected, err.Error())
		return err
	}
	sch(api.ConnectionConnected, "")
	return nil
}

func (r *RedisSink) Validate(props map[string]any) error {
	c := &config{DataType: "string", Expiration: -1, KeyType: "single"}
	err := cast.MapToStruct(props, c)
	if err != nil {
		return err
	}
	if c.Db < 0 || c.Db > 15 {
		return fmt.Errorf("redisSink db should be in range 0-15")
	}
	if c.KeyType == "single" && c.Key == "" && c.Field == "" {
		return errors.New("redis sink must have key or field when KeyType is single")
	}
	if c.KeyType != "single" && c.KeyType != "multiple" {
		return errors.New("KeyType only support single or multiple")
	}
	if c.DataType != "string" && c.DataType != "list" {
		return errors.New("redis sink only support string or list data type")
	}
	r.c = c
	return nil
}

func (r *RedisSink) Ping(ctx api.StreamContext, props map[string]any) error {
	if err := r.Validate(props); err != nil {
		return err
	}
	cli := redis.NewClient(&redis.Options{
		Addr:     r.c.Addr,
		Username: r.c.Username,
		Password: r.c.Password,
		DB:       r.c.Db, // use default DB
	})
	_, err := cli.Ping(ctx).Result()
	defer func() {
		cli.Close()
	}()
	return err
}

func (r *RedisSink) Collect(ctx api.StreamContext, item api.MessageTuple) error {
	return r.save(ctx, item.ToMap())
}

func (r *RedisSink) CollectList(ctx api.StreamContext, items api.MessageTupleList) error {
	// TODO handle partial error
	items.RangeOfTuples(func(_ int, tuple api.MessageTuple) bool {
		err := r.save(ctx, tuple.ToMap())
		if err != nil {
			ctx.GetLogger().Error(err)
		}
		return true
	})
	return nil
}

func (r *RedisSink) Close(ctx api.StreamContext) error {
	ctx.GetLogger().Infof("Closing redis sink")
	err := r.cli.Close()
	return err
}

func (r *RedisSink) save(ctx api.StreamContext, data map[string]any) error {
	logger := ctx.GetLogger()
	// prepare key value pairs
	values := make(map[string]string)
	if r.c.KeyType == "multiple" {
		for key, val := range data {
			v, _ := cast.ToString(val, cast.CONVERT_ALL)
			values[key] = v
		}
	} else {
		jsonBytes, err := json.Marshal(data)
		if err != nil {
			return err
		}
		val := string(jsonBytes)
		key := r.c.Key
		if r.c.Field != "" {
			keyval, ok := data[r.c.Field]
			if !ok {
				return fmt.Errorf("field %s does not exist in data %v", r.c.Field, data)
			}
			key, err = cast.ToString(keyval, cast.CONVERT_ALL)
			if err != nil {
				return fmt.Errorf("key must be string or convertible to string, but got %v", keyval)
			}
		}
		values[key] = val
	}
	// get action type
	rowkind := ast.RowkindUpsert
	if r.c.RowkindField != "" {
		c, ok := data[r.c.RowkindField]
		if ok {
			rowkind, ok = c.(string)
			if !ok {
				return fmt.Errorf("rowkind field %s is not a string in data %v", r.c.RowkindField, data)
			}
			if rowkind != ast.RowkindInsert && rowkind != ast.RowkindUpdate && rowkind != ast.RowkindDelete && rowkind != ast.RowkindUpsert {
				return fmt.Errorf("invalid rowkind %s", rowkind)
			}
		}
	}
	// set key value pairs
	for key, val := range values {
		var err error
		switch rowkind {
		case ast.RowkindInsert, ast.RowkindUpdate, ast.RowkindUpsert:
			if r.c.DataType == "list" {
				err = r.cli.LPush(ctx, key, val).Err()
				if err != nil {
					return fmt.Errorf("lpush %s:%s error, %v", key, val, err)
				}
				logger.Debugf("push redis list success, key:%s data: %v", key, val)
			} else {
				err = r.cli.Set(ctx, key, val, time.Duration(r.c.Expiration)).Err()
				if err != nil {
					return fmt.Errorf("set %s:%s error, %v", key, val, err)
				}
				logger.Debugf("set redis string success, key:%s data: %s", key, val)
			}
		case ast.RowkindDelete:
			if r.c.DataType == "list" {
				err = r.cli.LPop(ctx, key).Err()
				if err != nil {
					return fmt.Errorf("lpop %s error, %v", key, err)
				}
				logger.Debugf("pop redis list success, key:%s data: %v", key, val)
			} else {
				err = r.cli.Del(ctx, key).Err()
				if err != nil {
					logger.Error(err)
					return err
				}
				logger.Debugf("delete redis string success, key:%s data: %s", key, val)
			}
		default:
			// never happen
			logger.Errorf("unexpected rowkind %s", rowkind)
		}
	}
	return nil
}

func GetSink() api.Sink {
	return &RedisSink{}
}

var (
	_ api.TupleCollector = &RedisSink{}
	_ util.PingableConn  = &RedisSink{}
)
