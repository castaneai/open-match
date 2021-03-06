// Copyright 2020 Google LLC
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

package statestore

import (
	"context"
	"strconv"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/gomodule/redigo/redis"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"open-match.dev/open-match/internal/ipb"
	"open-match.dev/open-match/pkg/pb"
)

const (
	backfillLastAckTime = "backfill_last_ack_time"
	allBackfills        = "allBackfills"
)

// CreateBackfill creates a new Backfill in the state storage if one doesn't exist. The xids algorithm used to create the ids ensures that they are unique with no system wide synchronization. Calling clients are forbidden from choosing an id during create. So no conflicts will occur.
func (rb *redisBackend) CreateBackfill(ctx context.Context, backfill *pb.Backfill, ticketIDs []string) error {
	redisConn, err := rb.redisPool.GetContext(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "CreateBackfill, id: %s, failed to connect to redis: %v", backfill.GetId(), err)
	}
	defer handleConnectionClose(&redisConn)

	bf := ipb.BackfillInternal{
		Backfill:  backfill,
		TicketIds: ticketIDs,
	}

	value, err := proto.Marshal(&bf)
	if err != nil {
		err = errors.Wrapf(err, "failed to marshal the backfill proto, id: %s", backfill.GetId())
		return status.Errorf(codes.Internal, "%v", err)
	}

	res, err := redisConn.Do("SETNX", backfill.GetId(), value)
	if err != nil {
		err = errors.Wrapf(err, "failed to set the value for backfill, id: %s", backfill.GetId())
		return status.Errorf(codes.Internal, "%v", err)
	}

	if res.(int64) == 0 {
		return status.Errorf(codes.AlreadyExists, "backfill already exists, id: %s", backfill.GetId())
	}

	return acknowledgeBackfill(redisConn, backfill.GetId())
}

// GetBackfill gets the Backfill with the specified id from state storage. This method fails if the Backfill does not exist. Returns the Backfill and associated ticketIDs if they exist.
func (rb *redisBackend) GetBackfill(ctx context.Context, id string) (*pb.Backfill, []string, error) {
	redisConn, err := rb.redisPool.GetContext(ctx)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "GetBackfill, id: %s, failed to connect to redis: %v", id, err)
	}
	defer handleConnectionClose(&redisConn)

	value, err := redis.Bytes(redisConn.Do("GET", id))
	if err != nil {
		// Return NotFound if redigo did not find the backfill in storage.
		if err == redis.ErrNil {
			return nil, nil, status.Errorf(codes.NotFound, "Backfill id: %s not found", id)
		}

		err = errors.Wrapf(err, "failed to get the backfill from state storage, id: %s", id)
		return nil, nil, status.Errorf(codes.Internal, "%v", err)
	}

	if value == nil {
		return nil, nil, status.Errorf(codes.NotFound, "Backfill id: %s not found", id)
	}

	bi := &ipb.BackfillInternal{}
	err = proto.Unmarshal(value, bi)
	if err != nil {
		err = errors.Wrapf(err, "failed to unmarshal internal backfill, id: %s", id)
		return nil, nil, status.Errorf(codes.Internal, "%v", err)
	}

	return bi.Backfill, bi.TicketIds, nil
}

// DeleteBackfill removes the Backfill with the specified id from state storage. This method succeeds if the Backfill does not exist.
func (rb *redisBackend) DeleteBackfill(ctx context.Context, id string) error {
	redisConn, err := rb.redisPool.GetContext(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "DeleteBackfill, id: %s, failed to connect to redis: %v", id, err)
	}
	defer handleConnectionClose(&redisConn)

	_, err = redisConn.Do("DEL", id)
	if err != nil {
		err = errors.Wrapf(err, "failed to delete the backfill from state storage, id: %s", id)
		return status.Errorf(codes.Internal, "%v", err)
	}

	return rb.deleteExpiredBackfillID(redisConn, id)
}

// UpdateBackfill updates an existing Backfill with a new data. ticketIDs can be nil.
func (rb *redisBackend) UpdateBackfill(ctx context.Context, backfill *pb.Backfill, ticketIDs []string) error {
	redisConn, err := rb.redisPool.GetContext(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "UpdateBackfill, id: %s, failed to connect to redis: %v", backfill.GetId(), err)
	}
	defer handleConnectionClose(&redisConn)

	bf := ipb.BackfillInternal{
		Backfill:  backfill,
		TicketIds: ticketIDs,
	}

	value, err := proto.Marshal(&bf)
	if err != nil {
		err = errors.Wrapf(err, "failed to marshal the backfill proto, id: %s", backfill.GetId())
		return status.Errorf(codes.Internal, "%v", err)
	}

	_, err = redisConn.Do("SET", backfill.GetId(), value)
	if err != nil {
		err = errors.Wrapf(err, "failed to set the value for backfill, id: %s", backfill.GetId())
		return status.Errorf(codes.Internal, "%v", err)
	}

	return nil
}

// AcknowledgeBackfill stores Backfill's last acknowledgement time.
// Check on Backfill existence should be performed on Frontend side
func (rb *redisBackend) AcknowledgeBackfill(ctx context.Context, id string) error {
	redisConn, err := rb.redisPool.GetContext(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "AcknowledgeBackfill, id: %s, failed to connect to redis: %v", id, err)
	}
	defer handleConnectionClose(&redisConn)
	return acknowledgeBackfill(redisConn, id)
}

func acknowledgeBackfill(conn redis.Conn, backfillID string) error {
	currentTime := time.Now().UnixNano()

	_, err := conn.Do("ZADD", backfillLastAckTime, currentTime, backfillID)
	if err != nil {
		return status.Errorf(codes.Internal, "%v",
			errors.Wrap(err, "failed to store backfill's last acknowledgement time"))
	}

	return nil

}

// GetExpiredBackfillIDs gets all backfill IDs which are expired
func (rb *redisBackend) GetExpiredBackfillIDs(ctx context.Context) ([]string, error) {
	redisConn, err := rb.redisPool.GetContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "GetExpiredBackfillIDs, failed to connect to redis: %v", err)
	}
	defer handleConnectionClose(&redisConn)

	// Use a fraction 80% of pendingRelease Tickets TTL
	ttl := rb.cfg.GetDuration("pendingReleaseTimeout") / 5 * 4
	curTime := time.Now()
	endTimeInt := curTime.Add(-ttl).UnixNano()
	startTimeInt := 0

	// Filter out backfill IDs that are fetched but not assigned within TTL time (ms).
	expiredBackfillIds, err := redis.Strings(redisConn.Do("ZRANGEBYSCORE", backfillLastAckTime, startTimeInt, endTimeInt))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "error getting expired backfills %v", err)
	}

	return expiredBackfillIds, nil
}

// deleteExpiredBackfillID deletes expired BackfillID from a sorted set
func (rb *redisBackend) deleteExpiredBackfillID(conn redis.Conn, backfillID string) error {

	_, err := conn.Do("ZREM", backfillLastAckTime, backfillID)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to delete expired backfill ID %s from Sorted Set %s",
			backfillID, err.Error())
	}
	return nil
}

// IndexBackfill adds the backfill to the index.
func (rb *redisBackend) IndexBackfill(ctx context.Context, backfill *pb.Backfill) error {
	redisConn, err := rb.redisPool.GetContext(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "IndexBackfill, id: %s, failed to connect to redis: %v", backfill.GetId(), err)
	}
	defer handleConnectionClose(&redisConn)

	err = redisConn.Send("HSET", allBackfills, backfill.Id, backfill.Generation)
	if err != nil {
		err = errors.Wrapf(err, "failed to add backfill to all backfills, id: %s", backfill.Id)
		return status.Errorf(codes.Internal, "%v", err)
	}

	return nil
}

// DeindexBackfill removes specified Backfill ID from the index. The Backfill continues to exist.
func (rb *redisBackend) DeindexBackfill(ctx context.Context, id string) error {
	redisConn, err := rb.redisPool.GetContext(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "DeindexBackfill, id: %s, failed to connect to redis: %v", id, err)
	}
	defer handleConnectionClose(&redisConn)

	err = redisConn.Send("HDEL", allBackfills, id)
	if err != nil {
		err = errors.Wrapf(err, "failed to remove ID from backfill index, id: %s", id)
		return status.Errorf(codes.Internal, "%v", err)
	}

	return nil
}

// GetIndexedBackfills returns the ids of all backfills currently indexed.
func (rb *redisBackend) GetIndexedBackfills(ctx context.Context) (map[string]int, error) {
	redisConn, err := rb.redisPool.GetContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "GetIndexedBackfills, failed to connect to redis: %v", err)
	}
	defer handleConnectionClose(&redisConn)

	bfIndex, err := redis.StringMap(redisConn.Do("HGETALL", allBackfills))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "error getting all indexed backfill ids %v", err)
	}

	r := make(map[string]int, len(bfIndex))
	for bfID, bfGeneration := range bfIndex {
		gen, err := strconv.Atoi(bfGeneration)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "error while parsing generation into number: %v", err)
		}
		r[bfID] = gen
	}

	return r, nil
}
