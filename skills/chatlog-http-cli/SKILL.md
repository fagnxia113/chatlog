# Chatlog HTTP CLI Skill

## Purpose
Use CLI to call all Chatlog HTTP interfaces without opening browser.

## Command Entry
- `chatlog http list`
- `chatlog http call ...`

## Supported Endpoint Aliases
Run `chatlog http list` to get latest list.

Current aliases:
- `health` -> `GET /health`
- `ping` -> `GET /api/v1/ping`
- `sessions` -> `GET /api/v1/sessions`
- `history` -> `GET /api/v1/history`
- `search` -> `GET /api/v1/search`
- `unread` -> `GET /api/v1/unread`
- `members` -> `GET /api/v1/members`
- `new_messages` -> `GET /api/v1/new_messages`
- `stats` -> `GET /api/v1/stats`
- `favorites` -> `GET /api/v1/favorites`
- `sns_notifications` -> `GET /api/v1/sns_notifications`
- `sns_feed` -> `GET /api/v1/sns_feed`
- `sns_search` -> `GET /api/v1/sns_search`
- `contacts` -> `GET /api/v1/contacts`
- `chatrooms` -> `GET /api/v1/chatrooms`
- `db` -> `GET /api/v1/db`
- `db_tables` -> `GET /api/v1/db/tables`
- `db_data` -> `GET /api/v1/db/data`
- `db_query` -> `GET /api/v1/db/query`
- `cache_clear` -> `POST /api/v1/cache/clear`
- `image` -> `GET /image/{key}`
- `video` -> `GET /video/{key}`
- `file` -> `GET /file/{key}`
- `voice` -> `GET /voice/{key}`
- `data` -> `GET /data/{path}`
- `mcp` -> `POST /mcp`
- `mcp_sse` -> `GET /sse`
- `mcp_message` -> `POST /message`

## Common Flags
- `--addr` server address, default `127.0.0.1:5030`
- `--timeout` request timeout in seconds
- `--show-status` print HTTP status
- `--output` save response body to file

`call` flags:
- `--endpoint` endpoint alias
- `--path` raw path
- `--method` HTTP method
- `--query key=value` repeatable
- `--path-param key=value` replace `{key}` in path template
- `--header key=value` repeatable
- `--body` raw request body
- `--body-file` request body file

## Examples
```bash
# list endpoints
chatlog http list

# history
chatlog http call --endpoint history --query chat=华服饲料运输信息群 --query limit=100 --query format=json

# search
chatlog http call --endpoint search --query keyword=图片 --query limit=20

# db query
chatlog http call --endpoint db_query \
  --query group=message \
  --query file=message_0.db \
  --query sql='select local_id,create_time from MSG limit 5'

# media by key
chatlog http call --endpoint image --path-param key=b309f8f81716aff9e1c9dded3bec74e7

# clear cache
chatlog http call --endpoint cache_clear --method POST
```

## Notes
- Most `/api/v1/*` endpoints default to YAML output unless `format=json` is provided.
- `--endpoint` and `--path` can be used together; `--path` overrides endpoint path.
