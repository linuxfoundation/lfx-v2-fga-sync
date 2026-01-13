#!/bin/bash

# Test script for generic FGA handlers
# This script sends test messages to the new generic NATS subjects

set -e

echo "=========================================="
echo "Testing Generic FGA Handlers"
echo "=========================================="
echo ""

# NATS connection
NATS_URL="${NATS_URL:-nats://lfx-platform-nats.lfx.svc.cluster.local:4222}"
TIMEOUT="5s"

echo "Using NATS URL: $NATS_URL"
echo ""

# Check if NATS CLI is available
if ! command -v nats &> /dev/null; then
    echo "Error: nats CLI tool not found. Please install it first."
    echo "Visit: https://github.com/nats-io/natscli"
    exit 1
fi

# Test 1: Generic Update Access
echo "=========================================="
echo "Test 1: Generic Update Access (Committee)"
echo "=========================================="
nats request lfx.fga-sync.update_access --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"committee","operation":"update_access","data":{"uid":"test-committee-123","public":true,"relations":{"member":["user1","user2","user3"],"viewer":["user4"]},"references":{"parent":["committee-456"],"project":["project-789"]}}}'
echo ""

# Test 2: Generic Update Access with Exclude Relations
echo "=========================================="
echo "Test 2: Generic Update Access (Meeting with exclude_relations)"
echo "=========================================="
nats request lfx.fga-sync.update_access --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"meeting","operation":"update_access","data":{"uid":"test-meeting-456","public":false,"relations":{"organizer":["user1","user2"]},"references":{"project":["project-123"]},"exclude_relations":["participant","host"]}}'
echo ""

# Test 3: Generic Member Put (Single Relation)
echo "=========================================="
echo "Test 3: Generic Member Put (Single Relation)"
echo "=========================================="
nats request lfx.fga-sync.member_put --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"committee","operation":"member_put","data":{"uid":"test-committee-123","username":"user-alice","relations":["member"]}}'
echo ""

# Test 4: Generic Member Put (Multiple Relations)
echo "=========================================="
echo "Test 4: Generic Member Put (Multiple Relations)"
echo "=========================================="
nats request lfx.fga-sync.member_put --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"past_meeting","operation":"member_put","data":{"uid":"test-past-meeting-789","username":"user-bob","relations":["host","invitee"]}}'
echo ""

# Test 5: Generic Member Put (Mutually Exclusive Relations)
echo "=========================================="
echo "Test 5: Generic Member Put (Mutually Exclusive - Host replaces Participant)"
echo "=========================================="
nats request lfx.fga-sync.member_put --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"meeting","operation":"member_put","data":{"uid":"test-meeting-456","username":"user-charlie","relations":["host"],"mutually_exclusive_with":["participant","host"]}}'
echo ""

# Test 6: Generic Member Put (Idempotency Test - Same Member Again)
echo "=========================================="
echo "Test 6: Generic Member Put (Idempotency - Adding same member again)"
echo "=========================================="
nats request lfx.fga-sync.member_put --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"committee","operation":"member_put","data":{"uid":"test-committee-123","username":"user-alice","relations":["member"]}}'
echo ""

# Test 7: Generic Member Remove (Specific Relations)
echo "=========================================="
echo "Test 7: Generic Member Remove (Specific Relations)"
echo "=========================================="
nats request lfx.fga-sync.member_remove --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"past_meeting","operation":"member_remove","data":{"uid":"test-past-meeting-789","username":"user-bob","relations":["invitee"]}}'
echo ""

# Test 8: Generic Member Remove (All Relations)
echo "=========================================="
echo "Test 8: Generic Member Remove (All Relations - Empty Array)"
echo "=========================================="
nats request lfx.fga-sync.member_remove --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"committee","operation":"member_remove","data":{"uid":"test-committee-123","username":"user-alice","relations":[]}}'
echo ""

# Test 9: Generic Delete Access
echo "=========================================="
echo "Test 9: Generic Delete Access (Committee)"
echo "=========================================="
nats request lfx.fga-sync.delete_access --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"committee","operation":"delete_access","data":{"uid":"test-committee-123"}}'
echo ""

# Test 10: Generic Delete Access (Meeting)
echo "=========================================="
echo "Test 10: Generic Delete Access (Meeting)"
echo "=========================================="
nats request lfx.fga-sync.delete_access --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"meeting","operation":"delete_access","data":{"uid":"test-meeting-456"}}'
echo ""

# Test 11: Generic Delete Access (Past Meeting)
echo "=========================================="
echo "Test 11: Generic Delete Access (Past Meeting)"
echo "=========================================="
nats request lfx.fga-sync.delete_access --server="$NATS_URL" --timeout="$TIMEOUT" \
  '{"object_type":"past_meeting","operation":"delete_access","data":{"uid":"test-past-meeting-789"}}'
echo ""

echo "=========================================="
echo "All Generic Handler Tests Completed!"
echo "=========================================="
echo ""
echo "Summary of expected results:"
echo "  ✓ Test 1-2: Access updates with relations and references"
echo "  ✓ Test 3-4: Member additions (single and multiple relations)"
echo "  ✓ Test 5: Automatic cleanup of mutually exclusive relations"
echo "  ✓ Test 6: Idempotency check (should log 'no changes needed')"
echo "  ✓ Test 7-8: Member removals (specific and all relations)"
echo "  ✓ Test 9-11: Complete deletion of all tuples for resources"
