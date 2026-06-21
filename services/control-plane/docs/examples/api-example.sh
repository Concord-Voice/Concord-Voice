#!/bin/bash
# Concord API Example Script
# This script demonstrates the basic API flow

API_URL="http://localhost:8080/api/v1"
CONTENT_TYPE_JSON="Content-Type: application/json"

echo "=== Concord API Example ==="
echo

# 1. Register a new user
echo "1. Registering new user..."
# Substitute a password that meets the complexity policy (>=8 chars with
# upper, lower, digit, and symbol). This is an example placeholder — never
# commit real credentials.
REGISTER_RESPONSE=$(curl -s -X POST "$API_URL/auth/register" \
  -H "$CONTENT_TYPE_JSON" \
  -d '{
    "email": "test@example.com",
    "username": "testuser",
    "password": "<REPLACE_WITH_A_STRONG_PASSWORD>"
  }')

echo "$REGISTER_RESPONSE" | jq '.'
ACCESS_TOKEN=$(echo "$REGISTER_RESPONSE" | jq -r '.access_token')
echo

# 2. Create a server
echo "2. Creating a server..."
SERVER_RESPONSE=$(curl -s -X POST "$API_URL/servers" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H "$CONTENT_TYPE_JSON" \
  -d '{
    "name": "Test Server",
    "icon_url": "https://example.com/icon.png"
  }')

echo "$SERVER_RESPONSE" | jq '.'
SERVER_ID=$(echo "$SERVER_RESPONSE" | jq -r '.server.id')
echo

# 3. Create a channel
echo "3. Creating a channel..."
CHANNEL_RESPONSE=$(curl -s -X POST "$API_URL/channels" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H "$CONTENT_TYPE_JSON" \
  -d "{
    \"server_id\": \"$SERVER_ID\",
    \"name\": \"general\",
    \"type\": \"text\",
    \"description\": \"General discussion\"
  }")

echo "$CHANNEL_RESPONSE" | jq '.'
CHANNEL_ID=$(echo "$CHANNEL_RESPONSE" | jq -r '.channel.id')
echo

# 4. Send a message
echo "4. Sending a message..."
MESSAGE_RESPONSE=$(curl -s -X POST "$API_URL/messages" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H "$CONTENT_TYPE_JSON" \
  -d "{
    \"channel_id\": \"$CHANNEL_ID\",
    \"content\": \"Hello, Concord!\"
  }")

echo "$MESSAGE_RESPONSE" | jq '.'
echo

# 5. Get messages
echo "5. Getting messages..."
curl -s -X GET "$API_URL/channels/$CHANNEL_ID/messages?limit=10" \
  -H "Authorization: Bearer $ACCESS_TOKEN" | jq '.'
echo

# 6. List active sessions
echo "6. Listing active sessions..."
curl -s -X GET "$API_URL/sessions" \
  -H "Authorization: Bearer $ACCESS_TOKEN" | jq '.'
echo

echo "=== Example Complete ==="
echo "Access Token: $ACCESS_TOKEN"
echo "Server ID: $SERVER_ID"
echo "Channel ID: $CHANNEL_ID"
