package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/TykTechnologies/leakybucket"
	"github.com/TykTechnologies/leakybucket/memorycache"
	"github.com/TykTechnologies/tyk/config"
	"github.com/TykTechnologies/tyk/storage"
	"github.com/TykTechnologies/tyk/user"
)

type PublicSession struct {
	Quota struct {
		QuotaMax       int64 `json:"quota_max"`
		QuotaRemaining int64 `json:"quota_remaining"`
		QuotaRenews    int64 `json:"quota_renews"`
	} `json:"quota"`
	RateLimit struct {
		Rate float64 `json:"requests"`
		Per  float64 `json:"per_unit"`
	} `json:"rate_limit"`
}

const (
	QuotaKeyPrefix     = "quota-"
	RateLimitKeyPrefix = "rate-limit-"

	quotaChanBufferSize = 256
)

type keyQuota struct {
	key        string
	session    *user.SessionState
	isExceeded bool
	reqChan    chan bool
}

// SessionLimiter is the rate limiter for the API, use ForwardMessage() to
// check if a message should pass through or not
type SessionLimiter struct {
	bucketStore leakybucket.Storage

	keyQuotas   map[string]keyQuota
	keyQuotasMu sync.Mutex
}

func (l *SessionLimiter) getKeyQuota(key string, session *user.SessionState, store storage.Handler, globalConf *config.Config) keyQuota {
	l.keyQuotasMu.Lock()
	defer l.keyQuotasMu.Unlock()

	// check if key quota counter is already running
	if quota, ok := l.keyQuotas[key]; ok {
		return quota
	}

	// create and start new key quota counter
	quota := keyQuota{
		key:     key,
		session: session,
		reqChan: make(chan bool, quotaChanBufferSize),
	}
	l.keyQuotas[key] = quota
	syncEvery := time.Duration(session.QuotaRenewalRate*1000/int64(globalConf.DistributedQuotaSyncFrequency)) * time.Millisecond
	go l.startCounter(quota, store, syncEvery)

	return quota
}

func (l *SessionLimiter) setQuotaIsExceeded(key string, isExceeded bool) {
	l.keyQuotasMu.Lock()
	defer l.keyQuotasMu.Unlock()
	if quota, ok := l.keyQuotas[key]; ok {
		quota.isExceeded = isExceeded
		l.keyQuotas[key] = quota
	}
}

func (l *SessionLimiter) startCounter(quota keyQuota, store storage.Handler, syncEvery time.Duration) {
	// initialize total counter with current value from centralized Redis storage
	// by supplying 0 increment
	totalCounter := store.IncrementByWithExpire(quota.key, 0, quota.session.QuotaRenewalRate)

	// this var will be used to count requests per aggregation period
	var localCounter int64

	for {
		select {
		case _, ok := <-quota.reqChan:
			// check if channel was closed and it is time to stop go-routine
			if !ok {
				return
			}

			// check if quota was exceeded
			if totalCounter >= quota.session.QuotaMax {
				l.setQuotaIsExceeded(quota.key, true)
				localCounter = 0
				continue
			}

			// increment last known total counter and local aggregated counter
			totalCounter++
			localCounter++

			// update remaining
			remaining := quota.session.QuotaMax - totalCounter
			if remaining < 0 {
				quota.session.QuotaRemaining = 0
			} else {
				quota.session.QuotaRemaining = remaining
			}

		case <-time.After(syncEvery):
			// aggregation period is done
			// overwrite totalCounter from centralized Redis storage
			totalCounter = store.IncrementByWithExpire(quota.key, localCounter, quota.session.QuotaRenewalRate)
			// reset
			localCounter = 0
			l.setQuotaIsExceeded(quota.key, false)
		}
	}
}

func (l *SessionLimiter) stopCounters() {
	l.keyQuotasMu.Lock()
	defer l.keyQuotasMu.Unlock()
	for _, quota := range l.keyQuotas {
		close(quota.reqChan)
	}
}

func (l *SessionLimiter) doRollingWindowWrite(key, rateLimiterKey, rateLimiterSentinelKey string,
	currentSession *user.SessionState,
	store storage.Handler,
	globalConf *config.Config,
	apiLimit *user.APILimit) bool {

	var per, rate float64

	if apiLimit != nil { // respect limit on API level
		per = apiLimit.Per
		rate = apiLimit.Rate
	} else {
		per = currentSession.Per
		rate = currentSession.Rate
	}

	log.Debug("[RATELIMIT] Inbound raw key is: ", key)
	log.Debug("[RATELIMIT] Rate limiter key is: ", rateLimiterKey)
	pipeline := globalConf.EnableNonTransactionalRateLimiter
	ratePerPeriodNow, _ := store.SetRollingWindow(rateLimiterKey, int64(per), "-1", pipeline)

	//log.Info("Num Requests: ", ratePerPeriodNow)

	// Subtract by 1 because of the delayed add in the window
	subtractor := 1
	if globalConf.EnableSentinelRateLImiter {
		// and another subtraction because of the preemptive limit
		subtractor = 2
	}
	// The test TestRateLimitForAPIAndRateLimitAndQuotaCheck
	// will only work with ththese two lines here
	//log.Info("break: ", (int(currentSession.Rate) - subtractor))
	if ratePerPeriodNow > int(rate)-subtractor {
		// Set a sentinel value with expire
		if globalConf.EnableSentinelRateLImiter {
			store.SetRawKey(rateLimiterSentinelKey, "1", int64(per))
		}
		return true
	}

	return false
}

type sessionFailReason uint

const (
	sessionFailNone sessionFailReason = iota
	sessionFailRateLimit
	sessionFailQuota
)

// ForwardMessage will enforce rate limiting, returning a non-zero
// sessionFailReason if session limits have been exceeded.
// Key values to manage rate are Rate and Per, e.g. Rate of 10 messages
// Per 10 seconds
func (l *SessionLimiter) ForwardMessage(r *http.Request, currentSession *user.SessionState, key string, store storage.Handler, enableRL, enableQ bool, globalConf *config.Config, apiID string) sessionFailReason {
	if enableRL {
		// check for limit on API level (set to session by ApplyPolicies)
		var apiLimit *user.APILimit
		if len(currentSession.AccessRights) > 0 {
			if rights, ok := currentSession.AccessRights[apiID]; !ok {
				log.WithField("apiID", apiID).Debug("[RATE] unexpected apiID")
				return sessionFailRateLimit
			} else {
				apiLimit = rights.Limit
			}
		}

		if globalConf.EnableSentinelRateLImiter {
			rateLimiterKey := RateLimitKeyPrefix + currentSession.KeyHash()
			rateLimiterSentinelKey := RateLimitKeyPrefix + currentSession.KeyHash() + ".BLOCKED"
			if apiLimit != nil {
				rateLimiterKey = RateLimitKeyPrefix + apiID + "-" + currentSession.KeyHash()
				rateLimiterSentinelKey = RateLimitKeyPrefix + apiID + "-" + currentSession.KeyHash() + ".BLOCKED"
			}

			go l.doRollingWindowWrite(key, rateLimiterKey, rateLimiterSentinelKey, currentSession, store, globalConf, apiLimit)

			// Check sentinel
			_, sentinelActive := store.GetRawKey(rateLimiterSentinelKey)
			if sentinelActive == nil {
				// Sentinel is set, fail
				return sessionFailRateLimit
			}
		} else if globalConf.EnableRedisRollingLimiter {
			rateLimiterKey := RateLimitKeyPrefix + currentSession.KeyHash()
			rateLimiterSentinelKey := RateLimitKeyPrefix + currentSession.KeyHash() + ".BLOCKED"
			if apiLimit != nil {
				rateLimiterKey = RateLimitKeyPrefix + apiID + "-" + currentSession.KeyHash()
				rateLimiterSentinelKey = RateLimitKeyPrefix + apiID + "-" + currentSession.KeyHash() + ".BLOCKED"
			}

			if l.doRollingWindowWrite(key, rateLimiterKey, rateLimiterSentinelKey, currentSession, store, globalConf, apiLimit) {
				return sessionFailRateLimit
			}
		} else {
			// In-memory limiter
			if l.bucketStore == nil {
				l.bucketStore = memorycache.New()
			}

			// If a token has been updated, we must ensure we don't use
			// an old bucket an let the cache deal with it
			bucketKey := ""
			var currRate float64
			var per float64
			if apiLimit == nil {
				bucketKey = key + ":" + currentSession.LastUpdated
				currRate = currentSession.Rate
				per = currentSession.Per
			} else { // respect limit on API level
				bucketKey = apiID + ":" + key + ":" + currentSession.LastUpdated
				currRate = apiLimit.Rate
				per = apiLimit.Per
			}

			// DRL will always overflow with more servers on low rates
			rate := uint(currRate * float64(DRLManager.RequestTokenValue))
			if rate < uint(DRLManager.CurrentTokenValue) {
				rate = uint(DRLManager.CurrentTokenValue)
			}

			userBucket, err := l.bucketStore.Create(bucketKey,
				rate,
				time.Duration(per)*time.Second)
			if err != nil {
				log.Error("Failed to create bucket!")
				return sessionFailRateLimit
			}

			_, errF := userBucket.Add(uint(DRLManager.CurrentTokenValue))

			if errF != nil {
				return sessionFailRateLimit
			}
		}
	}

	if enableQ {
		if globalConf.LegacyEnableAllowanceCountdown {
			currentSession.Allowance--
		}

		if globalConf.DistributedQuotaEnabled {
			if l.DistributedRedisQuotaExceeded(currentSession, key, store, globalConf) {
				return sessionFailQuota
			}
		} else if l.RedisQuotaExceeded(r, currentSession, key, store, apiID) {
			return sessionFailQuota
		}
	}

	return sessionFailNone

}

func (l *SessionLimiter) RedisQuotaExceeded(r *http.Request, currentSession *user.SessionState, key string, store storage.Handler, apiID string) bool {
	log.Debug("[QUOTA] Inbound raw key is: ", key)

	// check for limit on API level (set to session by ApplyPolicies)
	var apiLimit *user.APILimit
	if len(currentSession.AccessRights) > 0 {
		if rights, ok := currentSession.AccessRights[apiID]; !ok {
			log.WithField("apiID", apiID).Debug("[QUOTA] unexpected apiID")
			return false
		} else {
			apiLimit = rights.Limit
		}
	}

	// Are they unlimited?
	if apiLimit == nil {
		if currentSession.QuotaMax == -1 {
			// No quota set
			return false
		}
	} else if apiLimit.QuotaMax == -1 {
		// No quota set
		return false
	}

	rawKey := ""
	var quotaRenewalRate int64
	var quotaRenews int64
	var quotaMax int64
	if apiLimit == nil {
		rawKey = QuotaKeyPrefix + currentSession.KeyHash()
		quotaRenewalRate = currentSession.QuotaRenewalRate
		quotaRenews = currentSession.QuotaRenews
		quotaMax = currentSession.QuotaMax
	} else {
		rawKey = QuotaKeyPrefix + apiID + "-" + currentSession.KeyHash()
		quotaRenewalRate = apiLimit.QuotaRenewalRate
		quotaRenews = apiLimit.QuotaRenews
		quotaMax = apiLimit.QuotaMax
	}

	log.Debug("[QUOTA] Quota limiter key is: ", rawKey)
	log.Debug("Renewing with TTL: ", quotaRenewalRate)
	// INCR the key (If it equals 1 - set EXPIRE)
	qInt := store.IncrememntWithExpire(rawKey, quotaRenewalRate)

	// if the returned val is >= quota: block
	if qInt-1 >= quotaMax {
		renewalDate := time.Unix(quotaRenews, 0)
		log.Debug("Renewal Date is: ", renewalDate)
		log.Debug("As epoch: ", quotaRenews)
		log.Debug("Session: ", currentSession)
		log.Debug("Now:", time.Now())
		if time.Now().After(renewalDate) {
			// The renewal date is in the past, we should update the quota!
			// Also, this fixes legacy issues where there is no TTL on quota buckets
			log.Warning("Incorrect key expiry setting detected, correcting")
			go store.DeleteRawKey(rawKey)
			qInt = 1
		} else {
			// Renewal date is in the future and the quota is exceeded
			return true
		}

	}

	// If this is a new Quota period, ensure we let the end user know
	if qInt == 1 {
		current := time.Now().Unix()
		if apiLimit == nil {
			currentSession.QuotaRenews = current + quotaRenewalRate
		} else {
			apiLimit.QuotaRenews = current + quotaRenewalRate
		}
		ctxScheduleSessionUpdate(r)
	}

	// If not, pass and set the values of the session to quotamax - counter
	remaining := quotaMax - qInt
	if remaining < 0 {
		remaining = 0
	}

	if apiLimit == nil {
		currentSession.QuotaRemaining = remaining
	} else {
		apiLimit.QuotaRemaining = remaining
	}

	return false
}

func (l *SessionLimiter) DistributedRedisQuotaExceeded(currentSession *user.SessionState, key string, store storage.Handler, globalConf *config.Config) bool {
	// Are they unlimited?
	if currentSession.QuotaMax == -1 {
		// No quota set
		return false
	}

	rawKey := QuotaKeyPrefix + currentSession.KeyHash()
	quotaCounter := l.getKeyQuota(rawKey, currentSession, store, globalConf)

	// count request if quota is not exceeded for the given key
	if !quotaCounter.isExceeded {
		quotaCounter.reqChan <- true
		return false
	}

	return true
}
