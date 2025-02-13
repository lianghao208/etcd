// Copyright 2022 The etcd Authors
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

// Package v3discovery provides an implementation of the cluster discovery that
// is used by etcd with v3 client.
package v3discovery

import (
	"context"
	"crypto/tls"
	"errors"

	"math"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.etcd.io/etcd/client/pkg/v3/transport"
	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/client/v3"

	"github.com/jonboulle/clockwork"
	"go.uber.org/zap"
)

const (
	discoveryPrefix = "/_etcd/registry"
)

var (
	ErrInvalidURL     = errors.New("discovery: invalid peer URL")
	ErrBadSizeKey     = errors.New("discovery: size key is bad")
	ErrSizeNotFound   = errors.New("discovery: size key not found")
	ErrFullCluster    = errors.New("discovery: cluster is full")
	ErrTooManyRetries = errors.New("discovery: too many retries")
)

var (
	// Number of retries discovery will attempt before giving up and error out.
	nRetries              = uint(math.MaxUint32)
	maxExponentialRetries = uint(8)
)

type DiscoveryConfig struct {
	Url string `json:"discovery"`

	DialTimeout      time.Duration `json:"discovery-dial-timeout"`
	RequestTimeOut   time.Duration `json:"discovery-request-timeout"`
	KeepAliveTime    time.Duration `json:"discovery-keepalive-time"`
	KeepAliveTimeout time.Duration `json:"discovery-keepalive-timeout"`

	InsecureTransport  bool   `json:"discovery-insecure-transport"`
	InsecureSkipVerify bool   `json:"discovery-insecure-skip-tls-verify"`
	CertFile           string `json:"discovery-cert"`
	KeyFile            string `json:"discovery-key"`
	TrustedCAFile      string `json:"discovery-cacert"`

	User     string `json:"discovery-user"`
	Password string `json:"discovery-password"`
}

type memberInfo struct {
	// peerRegKey is the key used by the member when registering in the
	// discovery service.
	// Format: "/_etcd/registry/<ClusterToken>/members/<memberId>".
	peerRegKey string
	// peerURLsMap format: "peerName=peerURLs", i.e., "member1=http://127.0.0.1:2380".
	peerURLsMap string
	// createRev is the member's CreateRevision in the etcd cluster backing
	// the discovery service.
	createRev int64
}

type clusterInfo struct {
	clusterToken string
	members      []memberInfo
}

// key prefix for each cluster: "/_etcd/registry/<ClusterToken>".
func geClusterKeyPrefix(cluster string) string {
	return path.Join(discoveryPrefix, cluster)
}

// key format for cluster size: "/_etcd/registry/<ClusterToken>/_config/size".
func geClusterSizeKey(cluster string) string {
	return path.Join(geClusterKeyPrefix(cluster), "_config/size")
}

// key prefix for each member: "/_etcd/registry/<ClusterToken>/members".
func getMemberKeyPrefix(clusterToken string) string {
	return path.Join(geClusterKeyPrefix(clusterToken), "members")
}

// key format for each member: "/_etcd/registry/<ClusterToken>/members/<memberId>".
func getMemberKey(cluster, memberId string) string {
	return path.Join(getMemberKeyPrefix(cluster), memberId)
}

// GetCluster will connect to the discovery service at the given url and
// retrieve a string describing the cluster
func GetCluster(lg *zap.Logger, dUrl string, cfg *DiscoveryConfig) (cs string, rerr error) {
	d, err := newDiscovery(lg, dUrl, cfg, 0)
	if err != nil {
		return "", err
	}

	defer d.close()
	defer func() {
		if rerr != nil {
			d.lg.Error(
				"discovery failed to get cluster",
				zap.String("cluster", cs),
				zap.Error(rerr),
			)
		} else {
			d.lg.Info(
				"discovery got cluster successfully",
				zap.String("cluster", cs),
			)
		}
	}()

	return d.getCluster()
}

// JoinCluster will connect to the discovery service at the given url, and
// register the server represented by the given id and config to the cluster.
// The parameter `config` is supposed to be in the format "memberName=peerURLs",
// such as "member1=http://127.0.0.1:2380".
//
// The final returned string has the same format as "--initial-cluster", such as
// "infra1=http://127.0.0.1:12380,infra2=http://127.0.0.1:22380,infra3=http://127.0.0.1:32380".
func JoinCluster(lg *zap.Logger, durl string, cfg *DiscoveryConfig, id types.ID, config string) (cs string, rerr error) {
	d, err := newDiscovery(lg, durl, cfg, id)
	if err != nil {
		return "", err
	}

	defer d.close()
	defer func() {
		if rerr != nil {
			d.lg.Error(
				"discovery failed to join cluster",
				zap.String("cluster", cs),
				zap.Error(rerr),
			)
		} else {
			d.lg.Info(
				"discovery joined cluster successfully",
				zap.String("cluster", cs),
			)
		}
	}()

	return d.joinCluster(config)
}

type discovery struct {
	lg           *zap.Logger
	clusterToken string
	memberId     types.ID
	c            *clientv3.Client
	retries      uint
	durl         string

	cfg *DiscoveryConfig

	clock clockwork.Clock
}

func newDiscovery(lg *zap.Logger, durl string, dcfg *DiscoveryConfig, id types.ID) (*discovery, error) {
	if lg == nil {
		lg = zap.NewNop()
	}
	u, err := url.Parse(durl)
	if err != nil {
		return nil, err
	}
	token := u.Path
	u.Path = ""

	lg = lg.With(zap.String("discovery-url", durl))
	cfg, err := newClientCfg(dcfg, u.String(), lg)
	if err != nil {
		return nil, err
	}

	c, err := clientv3.New(*cfg)
	if err != nil {
		return nil, err
	}
	return &discovery{
		lg:           lg,
		clusterToken: token,
		memberId:     id,
		c:            c,
		durl:         u.String(),
		cfg:          dcfg,
		clock:        clockwork.NewRealClock(),
	}, nil
}

// The following function follows the same logic as etcdctl, refer to
// https://github.com/etcd-io/etcd/blob/f9a8c49c695b098d66a07948666664ea10d01a82/etcdctl/ctlv3/command/global.go#L191-L250
func newClientCfg(dcfg *DiscoveryConfig, dUrl string, lg *zap.Logger) (*clientv3.Config, error) {
	var cfgtls *transport.TLSInfo

	if dcfg.CertFile != "" || dcfg.KeyFile != "" || dcfg.TrustedCAFile != "" {
		cfgtls = &transport.TLSInfo{
			CertFile:      dcfg.CertFile,
			KeyFile:       dcfg.KeyFile,
			TrustedCAFile: dcfg.TrustedCAFile,
			Logger:        lg,
		}
	}

	cfg := &clientv3.Config{
		Endpoints:            []string{dUrl},
		DialTimeout:          dcfg.DialTimeout,
		DialKeepAliveTime:    dcfg.KeepAliveTime,
		DialKeepAliveTimeout: dcfg.KeepAliveTimeout,
		Username:             dcfg.User,
		Password:             dcfg.Password,
	}

	if cfgtls != nil {
		if clientTLS, err := cfgtls.ClientConfig(); err == nil {
			cfg.TLS = clientTLS
		} else {
			return nil, err
		}
	}

	// If key/cert is not given but user wants secure connection, we
	// should still setup an empty tls configuration for gRPC to setup
	// secure connection.
	if cfg.TLS == nil && !dcfg.InsecureTransport {
		cfg.TLS = &tls.Config{}
	}

	// If the user wants to skip TLS verification then we should set
	// the InsecureSkipVerify flag in tls configuration.
	if cfg.TLS != nil && dcfg.InsecureSkipVerify {
		cfg.TLS.InsecureSkipVerify = true
	}

	return cfg, nil
}

func (d *discovery) getCluster() (string, error) {
	cls, clusterSize, rev, err := d.checkCluster()
	if err != nil {
		if err == ErrFullCluster {
			return cls.getInitClusterStr(clusterSize)
		}
		return "", err
	}

	for cls.Len() < clusterSize {
		d.waitPeers(cls, clusterSize, rev)
	}

	return cls.getInitClusterStr(clusterSize)
}

func (d *discovery) joinCluster(config string) (string, error) {
	_, _, _, err := d.checkCluster()
	if err != nil {
		return "", err
	}

	if err := d.registerSelf(config); err != nil {
		return "", err
	}

	cls, clusterSize, rev, err := d.checkCluster()
	if err != nil {
		return "", err
	}

	for cls.Len() < clusterSize {
		d.waitPeers(cls, clusterSize, rev)
	}

	return cls.getInitClusterStr(clusterSize)
}

func (d *discovery) getClusterSize() (int, error) {
	configKey := geClusterSizeKey(d.clusterToken)
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.RequestTimeOut)
	defer cancel()

	resp, err := d.c.Get(ctx, configKey)
	if err != nil {
		d.lg.Warn(
			"failed to get cluster size from discovery service",
			zap.String("clusterSizeKey", configKey),
			zap.Error(err),
		)
		return 0, err
	}

	if len(resp.Kvs) == 0 {
		return 0, ErrSizeNotFound
	}

	clusterSize, err := strconv.ParseInt(string(resp.Kvs[0].Value), 10, 0)
	if err != nil || clusterSize <= 0 {
		return 0, ErrBadSizeKey
	}

	return int(clusterSize), nil
}

func (d *discovery) getClusterMembers() (*clusterInfo, int64, error) {
	membersKeyPrefix := getMemberKeyPrefix(d.clusterToken)
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.RequestTimeOut)
	defer cancel()

	resp, err := d.c.Get(ctx, membersKeyPrefix, clientv3.WithPrefix())
	if err != nil {
		d.lg.Warn(
			"failed to get cluster members from discovery service",
			zap.String("membersKeyPrefix", membersKeyPrefix),
			zap.Error(err),
		)
		return nil, 0, err
	}

	cls := &clusterInfo{clusterToken: d.clusterToken}
	for _, kv := range resp.Kvs {
		mKey := strings.TrimSpace(string(kv.Key))
		mValue := strings.TrimSpace(string(kv.Value))

		if err := cls.add(mKey, mValue, kv.CreateRevision); err != nil {
			d.lg.Warn(
				err.Error(),
				zap.String("memberKey", mKey),
				zap.String("memberInfo", mValue),
			)
		} else {
			d.lg.Info(
				"found peer from discovery service",
				zap.String("memberKey", mKey),
				zap.String("memberInfo", mValue),
			)
		}
	}

	return cls, resp.Header.Revision, nil
}

func (d *discovery) checkClusterRetry() (*clusterInfo, int, int64, error) {
	if d.retries < nRetries {
		d.logAndBackoffForRetry("cluster status check")
		return d.checkCluster()
	}
	return nil, 0, 0, ErrTooManyRetries
}

func (d *discovery) checkCluster() (*clusterInfo, int, int64, error) {
	clusterSize, err := d.getClusterSize()
	if err != nil {
		if err == ErrSizeNotFound || err == ErrBadSizeKey {
			return nil, 0, 0, err
		}

		return d.checkClusterRetry()
	}

	cls, rev, err := d.getClusterMembers()
	if err != nil {
		return d.checkClusterRetry()
	}
	d.retries = 0

	// find self position
	memberSelfId := getMemberKey(d.clusterToken, d.memberId.String())
	idx := 0
	for _, m := range cls.members {
		if m.peerRegKey == memberSelfId {
			break
		}
		if idx >= clusterSize-1 {
			return cls, clusterSize, rev, ErrFullCluster
		}
		idx++
	}
	return cls, clusterSize, rev, nil
}

func (d *discovery) registerSelfRetry(contents string) error {
	if d.retries < nRetries {
		d.logAndBackoffForRetry("register member itself")
		return d.registerSelf(contents)
	}
	return ErrTooManyRetries
}

func (d *discovery) registerSelf(contents string) error {
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.RequestTimeOut)
	memberKey := getMemberKey(d.clusterToken, d.memberId.String())
	_, err := d.c.Put(ctx, memberKey, contents)
	cancel()

	if err != nil {
		d.lg.Warn(
			"failed to register members itself to the discovery service",
			zap.String("memberKey", memberKey),
			zap.Error(err),
		)
		return d.registerSelfRetry(contents)
	}
	d.retries = 0

	d.lg.Info(
		"register member itself successfully",
		zap.String("memberKey", memberKey),
		zap.String("memberInfo", contents),
	)

	return nil
}

func (d *discovery) waitPeers(cls *clusterInfo, clusterSize int, rev int64) {
	// watch from the next revision
	membersKeyPrefix := getMemberKeyPrefix(d.clusterToken)
	w := d.c.Watch(context.Background(), membersKeyPrefix, clientv3.WithPrefix(), clientv3.WithRev(rev+1))

	d.lg.Info(
		"waiting for peers from discovery service",
		zap.Int("clusterSize", clusterSize),
		zap.Int("found-peers", cls.Len()),
	)

	// waiting for peers until all needed peers are returned
	for wresp := range w {
		for _, ev := range wresp.Events {
			mKey := strings.TrimSpace(string(ev.Kv.Key))
			mValue := strings.TrimSpace(string(ev.Kv.Value))

			if err := cls.add(mKey, mValue, ev.Kv.CreateRevision); err != nil {
				d.lg.Warn(
					err.Error(),
					zap.String("memberKey", mKey),
					zap.String("memberInfo", mValue),
				)
			} else {
				d.lg.Info(
					"found peer from discovery service",
					zap.String("memberKey", mKey),
					zap.String("memberInfo", mValue),
				)
			}
		}

		if cls.Len() >= clusterSize {
			break
		}
	}

	d.lg.Info(
		"found all needed peers from discovery service",
		zap.Int("clusterSize", clusterSize),
		zap.Int("found-peers", cls.Len()),
	)
}

func (d *discovery) logAndBackoffForRetry(step string) {
	d.retries++
	// logAndBackoffForRetry stops exponential backoff when the retries are
	// more than maxExpoentialRetries and is set to a constant backoff afterward.
	retries := d.retries
	if retries > maxExponentialRetries {
		retries = maxExponentialRetries
	}
	retryTimeInSecond := time.Duration(0x1<<retries) * time.Second
	d.lg.Warn(
		"retry connecting to discovery service",
		zap.String("reason", step),
		zap.Duration("backoff", retryTimeInSecond),
	)
	d.clock.Sleep(retryTimeInSecond)
}

func (d *discovery) close() error {
	if d.c != nil {
		return d.c.Close()
	}
	return nil
}

func (cls *clusterInfo) Len() int { return len(cls.members) }
func (cls *clusterInfo) Less(i, j int) bool {
	return cls.members[i].createRev < cls.members[j].createRev
}
func (cls *clusterInfo) Swap(i, j int) {
	cls.members[i], cls.members[j] = cls.members[j], cls.members[i]
}

func (cls *clusterInfo) add(memberKey, memberValue string, rev int64) error {
	membersKeyPrefix := getMemberKeyPrefix(cls.clusterToken)

	if !strings.HasPrefix(memberKey, membersKeyPrefix) {
		// It should never happen because previously we used exactly the
		// same ${membersKeyPrefix} to get or watch the member list.
		return errors.New("invalid peer registry key")
	}

	if strings.IndexRune(memberValue, '=') == -1 {
		// It must be in the format "member1=http://127.0.0.1:2380".
		return errors.New("invalid peer info returned from discovery service")
	}

	if cls.exist(memberKey) {
		return errors.New("found duplicate peer from discovery service")
	}

	cls.members = append(cls.members, memberInfo{
		peerRegKey:  memberKey,
		peerURLsMap: memberValue,
		createRev:   rev,
	})

	// When multiple members register at the same time, then number of
	// registered members may be larger than the configured cluster size.
	// So we sort all the members on the CreateRevision in ascending order,
	// and get the first ${clusterSize} members in this case.
	sort.Sort(cls)

	return nil
}

func (cls *clusterInfo) exist(mKey string) bool {
	// Usually there are just a couple of members, so performance shouldn't be a problem.
	for _, m := range cls.members {
		if mKey == m.peerRegKey {
			return true
		}
	}
	return false
}

func (cls *clusterInfo) getInitClusterStr(clusterSize int) (string, error) {
	peerURLs := cls.getPeerURLs()

	if len(peerURLs) > clusterSize {
		peerURLs = peerURLs[:clusterSize]
	}

	us := strings.Join(peerURLs, ",")
	_, err := types.NewURLsMap(us)
	if err != nil {
		return us, ErrInvalidURL
	}

	return us, nil
}

func (cls *clusterInfo) getPeerURLs() []string {
	var peerURLs []string
	for _, peer := range cls.members {
		peerURLs = append(peerURLs, peer.peerURLsMap)
	}
	return peerURLs
}
