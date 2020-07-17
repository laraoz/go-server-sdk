package rediscluster

import (
	"encoding/json"
	"strings"
	"fmt"
	"time"
	"crypto/tls"

	r "github.com/go-redis/redis"

	ld "gopkg.in/launchdarkly/go-server-sdk.v4"
	"gopkg.in/launchdarkly/go-server-sdk.v4/ldlog"
	"gopkg.in/launchdarkly/go-server-sdk.v4/utils"
)

const (
	// DefaultAddr is the default address for connecting to Redis, if you use
	// If you are using the constructor, you must specify the Address explicitly.
	DefaultAddr = "localhost:6379"
	// DefaultPrefix is a string that is prepended (along with a colon) to all Redis keys used
	// by the feature store. You can change this value with the Prefix() option for
	// NewRedisFeatureStoreWithDefaults, or with the "prefix" parameter to the other constructors.
	DefaultPrefix = "launchdarkly"
	// DefaultCacheTTL is the default amount of time that recently read or updated items will
	// be cached in memory, if you use NewRedisFeatureStoreWithDefaults. You can specify otherwise
	// with the CacheTTL option. If you are using the other constructors, their "timeout"
	// parameter serves the same purpose and there is no default.
	DefaultCacheTTL = 15 * time.Second
)

type redisFeatureStoreOptions struct {
	prefix      string
	addr 	string
	password string
	cacheTTL    time.Duration
	logger      ld.Logger
}

// FeatureStoreOption is the interface for optional configuration parameters that can be
// passed to NewRedisFeatureStoreFactory. These include UseConfig, Prefix, CacheTTL, and UseLogger.
type FeatureStoreOption interface {
	apply(opts *redisFeatureStoreOptions) error
}

type prefixOption struct {
	prefix string
}

func (o prefixOption) apply(opts *redisFeatureStoreOptions) error {
	if o.prefix == "" {
		opts.prefix = DefaultPrefix
	} else {
		opts.prefix = o.prefix
	}
	return nil
}

// Prefix creates an option for NewRedisFeatureStoreFactory to specify a string
// that should be prepended to all Redis keys used by the feature store. A colon will be
// added to this automatically. If this is unspecified or empty, DefaultPrefix will be used.
//
//     factory, err := redis.NewRedisFeatureStoreFactory(redis.Prefix("ld-data"))
func Prefix(prefix string) FeatureStoreOption {
	return prefixOption{prefix}
}

type addrPassOption struct {
	addr string
	password string
}

func (o addrPassOption) apply(opts *redisFeatureStoreOptions) error {
	opts.addr = o.addr
	opts.password = o.password
	return nil
}

func AddrPassword(addr string, password string) FeatureStoreOption {
	return addrPassOption{addr: addr, password: password}
}


type cacheTTLOption struct {
	cacheTTL time.Duration
}

func (o cacheTTLOption) apply(opts *redisFeatureStoreOptions) error {
	opts.cacheTTL = o.cacheTTL
	return nil
}

// CacheTTL creates an option for NewRedisFeatureStoreFactory to set the amount of time
// that recently read or updated items should remain in an in-memory cache. This reduces the
// amount of database access if the same feature flags are being evaluated repeatedly.
//
// The default value is DefaultCacheTTL. A value of zero disables in-memory caching completely.
// A negative value means data is cached forever (i.e. it will only be read again from the
// database if the SDK is restarted). Use the "cached forever" mode with caution: it means
// that in a scenario where multiple processes are sharing the database, and the current
// process loses connectivity to LaunchDarkly while other processes are still receiving
// updates and writing them to the database, the current process will have stale data.
//
//     factory, err := redis.NewRedisFeatureStoreFactory(redis.CacheTTL(30*time.Second))
func CacheTTL(ttl time.Duration) FeatureStoreOption {
	return cacheTTLOption{ttl}
}

type loggerOption struct {
	logger ld.Logger
}

func (o loggerOption) apply(opts *redisFeatureStoreOptions) error {
	opts.logger = o.logger
	return nil
}

// Logger creates an option for NewRedisFeatureStore, to specify where to send log output.
//
// If you use NewConsulFeatureStoreFactory rather than the deprecated constructors, you do not
// need to specify a logger because it will use the same logging configuration as the SDK client.
//
//     store, err := redis.NewRedisFeatureStore(redis.Logger(myLogger))
func Logger(logger ld.Logger) FeatureStoreOption {
	return loggerOption{logger}
}

// RedisFeatureStore is a Redis-backed feature store implementation.
type RedisFeatureStore struct { // nolint:golint // package name in type name
	wrapper *utils.FeatureStoreWrapper
}

// redisFeatureStoreCore is the internal implementation, using the simpler interface defined in
// utils.FeatureStoreCore. The FeatureStoreWrapper wraps this to add caching. The only reason that
// there is a separate RedisFeatureStore type, instead of just using the FeatureStoreWrapper itself
// as the outermost object, is a historical one: the NewRedisFeatureStore constructors had already
// been defined as returning *RedisFeatureStore rather than the interface type.
type redisFeatureStoreCore struct {
	options    redisFeatureStoreOptions
	loggers    ldlog.Loggers
	pool       *r.ClusterClient
	testTxHook func()
}

func newPool(address, password string ) *r.ClusterClient {
	client := r.NewClusterClient(&r.ClusterOptions{
		Addrs:        []string{address},
		Password:     password,
		TLSConfig:    &tls.Config{},
		PoolSize:     3,
		DialTimeout:  time.Second * 10,
		ReadTimeout:  time.Second * 10,
		WriteTimeout: time.Second * 10,
	})

	// ping the server so we know we are good
	err := client.Ping().Err()
	if err != nil {
		return nil
	}


	return client
}

const (
	initedKey = "$inited"
	hashtag = "{ld}"
)

// NewRedisFeatureStoreFactory returns a factory function for a Redis-backed feature store.
//
// By default, it uses DefaultAddress as the Redis address, DefaultPrefix as the prefix for all keys,
// DefaultCacheTTL as the duration for in-memory caching, no authentication and a default connection
// pool configuration (see package description for details). You may override any of these with
// FeatureStoreOption values created with RedisAddrPassword, Prefix, CacheTTL,
// Logger, or Auth.
//
// Set the FeatureStoreFactory field in your Config to the returned value. Because this is specified
// as a factory function, the Redis client is not actually created until you create the SDK client.
// This also allows it to use the same logging configuration as the SDK, so you do not have to
// specify the Logger option separately.
func NewRedisFeatureStoreFactory(options ...FeatureStoreOption) (ld.FeatureStoreFactory, error) {
	configuredOptions, err := validateOptions(options...)
	if err != nil {
		return nil, err
	}
	return func(ldConfig ld.Config) (ld.FeatureStore, error) {
		core := newRedisFeatureStoreInternal(configuredOptions, ldConfig)
		return utils.NewFeatureStoreWrapperWithConfig(core, ldConfig), nil
	}, nil
}

func newStoreForDeprecatedConstructors(options ...FeatureStoreOption) *RedisFeatureStore {
	configuredOptions, err := validateOptions(options...)
	if err != nil {
		return nil
	}
	core := newRedisFeatureStoreInternal(configuredOptions, ld.Config{})
	return &RedisFeatureStore{wrapper: utils.NewFeatureStoreWrapperWithConfig(core, ld.Config{})}
}

func validateOptions(options ...FeatureStoreOption) (redisFeatureStoreOptions, error) {
	ret := redisFeatureStoreOptions{
		prefix:   DefaultPrefix,
		cacheTTL: DefaultCacheTTL,
	}
	for _, o := range options {
		err := o.apply(&ret)
		if err != nil {
			return ret, err
		}
	}
	return ret, nil
}

func newRedisFeatureStoreInternal(configuredOptions redisFeatureStoreOptions, ldConfig ld.Config) *redisFeatureStoreCore {
	core := &redisFeatureStoreCore{
		options: configuredOptions,
		loggers: ldConfig.Loggers, // copied by value so we can modify it
	}
	core.loggers.SetBaseLogger(configuredOptions.logger) // has no effect if it is nil
	core.loggers.SetPrefix("RedisFeatureStore:")

	if core.pool == nil {
		core.loggers.Infof("Using address: %s", configuredOptions.addr )
		core.pool = newPool(configuredOptions.addr, configuredOptions.password)
	}
	return core
}

// Get returns an individual object of a given type from the store
func (store *RedisFeatureStore) Get(kind ld.VersionedDataKind, key string) (ld.VersionedData, error) {
	return store.wrapper.Get(kind, key)
}

// All returns all the objects of a given kind from the store
func (store *RedisFeatureStore) All(kind ld.VersionedDataKind) (map[string]ld.VersionedData, error) {
	return store.wrapper.All(kind)
}

// Init populates the store with a complete set of versioned data
func (store *RedisFeatureStore) Init(allData map[ld.VersionedDataKind]map[string]ld.VersionedData) error {
	return store.wrapper.Init(allData)
}

// Upsert inserts or replaces an item in the store unless there it already contains an item with an equal or larger version
func (store *RedisFeatureStore) Upsert(kind ld.VersionedDataKind, item ld.VersionedData) error {
	return store.wrapper.Upsert(kind, item)
}

// Delete removes an item of a given kind from the store
func (store *RedisFeatureStore) Delete(kind ld.VersionedDataKind, key string, version int) error {
	return store.wrapper.Delete(kind, key, version)
}

// Initialized returns whether redis contains an entry for this environment
func (store *RedisFeatureStore) Initialized() bool {
	return store.wrapper.Initialized()
}

// Actual implementation methods are below - these are called by FeatureStoreWrapper, which adds
// caching behavior if necessary.

func (store *redisFeatureStoreCore) GetCacheTTL() time.Duration {
	return store.options.cacheTTL
}

func (store *redisFeatureStoreCore) GetInternal(kind ld.VersionedDataKind, key string) (ld.VersionedData, error) {
	c := store.getConn()

	fmt.Println()
	fmt.Println("Calling GetInternal")
	fmt.Println()
	jsonStr, err := c.HGet(store.featuresKey(kind), hashTagKey(key)).Result()
	if err != nil {
		if err == r.Nil {
			store.loggers.Debugf("Key: %s not found in \"%s\"", key, kind.GetNamespace())
			return nil, nil
		}
		return nil, err
	}

	item, jsonErr := utils.UnmarshalItem(kind, []byte(jsonStr))
	if jsonErr != nil {
		return nil, fmt.Errorf("failed to unmarshal %s key %s: %s", kind, key, jsonErr)
	}
	return item, nil
}

func (store *redisFeatureStoreCore) GetAllInternal(kind ld.VersionedDataKind) (map[string]ld.VersionedData, error) {
	results := make(map[string]ld.VersionedData)

	c := store.getConn()
	fmt.Println()
	fmt.Println("Getting all from GetAllInternal")
	fmt.Println()
	values, err := c.HGetAll(store.featuresKey(kind)).Result()
	if err != nil && err != r.Nil {
		return nil, err
	}

	for k, v := range values {
		item, jsonErr := utils.UnmarshalItem(kind, []byte(v))
		if jsonErr != nil {
			return nil, fmt.Errorf("failed to unmarshal %s: %s", kind, err)
		}

		results[removeHashTagKey(k)] = item
	}
	return results, nil
}

// Init populates the store with a complete set of versioned data
func (store *redisFeatureStoreCore) InitInternal(allData map[ld.VersionedDataKind]map[string]ld.VersionedData) error {
	c := store.getConn()

	//_ = c.Send("MULTI")
	pipe := c.Pipeline()

	for kind, items := range allData {
		baseKey := store.featuresKey(kind)

		_ = pipe.Del(baseKey).Err()

		for k, v := range items {
			data, jsonErr := json.Marshal(v)
			if jsonErr != nil {
				return fmt.Errorf("failed to marshal %s key %s: %s", kind, k, jsonErr)
			}

			_ = pipe.HSet(baseKey, hashTagKey(k), data)
		}
	}

	_ = pipe.Set(store.initedKey(), "", 0 )

	_, err := pipe.Exec()

	return err
}

func (store *redisFeatureStoreCore) UpsertInternal(kind ld.VersionedDataKind, newItem ld.VersionedData) (ld.VersionedData, error) {
	baseKey := store.featuresKey(kind)
	key := newItem.GetKey()
	var item ld.VersionedData
	shouldContinueExecution := false
	for {
		// We accept that we can acquire multiple connections here and defer inside loop but we don't expect many
		c := store.getConn()
		shouldContinueExecution = false
		err := c.Watch(func (tx *r.Tx) error {
			oldItem, err := store.GetInternal(kind, key)
			if err != nil {
				return err
			}

			if oldItem != nil && oldItem.GetVersion() >= newItem.GetVersion() {
				updateOrDelete := "update"
				if newItem.IsDeleted() {
					updateOrDelete = "delete"
				}
				store.loggers.Debugf(`Attempted to %s key: %s version: %d in "%s" with a version that is the same or older: %d`,
					updateOrDelete, key, oldItem.GetVersion(), kind.GetNamespace(), newItem.GetVersion())
				item = oldItem
				return nil
			}

			data, jsonErr := json.Marshal(newItem)
			if jsonErr != nil {
				return fmt.Errorf("failed to marshal %s key %s: %s", kind, key, jsonErr)
			}
			//fmt.Println()
			//fmt.Println()
			//fmt.Println("About to Sleep!!!!")
			//fmt.Println()
			//fmt.Println()
			//time.Sleep(2 *time.Minute)
			//fmt.Println()
			//fmt.Println()
			//fmt.Println("Waking up!!!!")
			//fmt.Println()
			//fmt.Println()

			pipe := tx.Pipeline()
			defer pipe.Close()
			//_ = c.Send("MULTI")
			//err = c.Send("HSET", baseKey, key, data)
			err = pipe.HSet(baseKey, hashTagKey(key), data).Err()
			if err == nil {
				fmt.Println()
				fmt.Println("HSET WORKED, ABOUT TO EXEC")
				fmt.Println()
				var result interface{}
				//result, err = c.Do("EXEC")
				result, err = pipe.Exec()
				if err == nil {
					if result == nil {
						fmt.Println()
						fmt.Println("Empty Result, meaning WATCH FAILED")
						fmt.Println()
						// if exec returned nothing, it means the watch was triggered and we should retry
						store.loggers.Debug("Concurrent modification detected, retrying")
						shouldContinueExecution = true
						return nil
					}
				}
				item = newItem
				return  nil
			}

			fmt.Println()
			fmt.Println("Seems that HSET failed on Upsert")
			fmt.Println()

			return  err
		}, baseKey )
		//_, err := c.Do("WATCH", baseKey)
		if err != nil {
			return nil, err
		}

		if !shouldContinueExecution {
			return item, nil
		}

		//defer c.Send("UNWATCH") // nolint:errcheck // this should always succeed

		//if store.testTxHook != nil { // instrumentation for unit tests
		//	store.testTxHook()
		//}
	}
}

func (store *redisFeatureStoreCore) InitializedInternal() bool {
	c := store.getConn()
	inited, _ := c.Exists(store.initedKey()).Result()
	return inited == 1
}

func (store *redisFeatureStoreCore) IsStoreAvailable() bool {
	c := store.getConn()
	_, err := c.Exists( store.initedKey()).Result()
	return err == nil
}

// Used internally to describe this component in diagnostic data.
func (store *redisFeatureStoreCore) GetDiagnosticsComponentTypeName() string {
	return "Redis"
}

func (store *redisFeatureStoreCore) featuresKey(kind ld.VersionedDataKind) string {
	return store.options.prefix + ":" + kind.GetNamespace() + "."+ hashtag
}

func (store *redisFeatureStoreCore) initedKey() string {
	return store.options.prefix + ":" + initedKey + "."+ hashtag
}

func hashTagKey(key string) string {
	return key + "."+ hashtag
}

func removeHashTagKey(key string) string {
	return strings.Replace(key, "." + hashtag, "",-1)
}

func (store *redisFeatureStoreCore) getConn() *r.ClusterClient {
	return store.pool
}