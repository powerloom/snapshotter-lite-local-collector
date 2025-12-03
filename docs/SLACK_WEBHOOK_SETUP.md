# Slack Webhook Setup Guide

## Quick Setup

### Option 1: Incoming Webhooks (Recommended - Simplest)

This is the simplest approach and doesn't require Workflow Builder:

1. **Go to Slack Apps**: https://api.slack.com/apps
2. **Create/Select App**: 
   - Click "Create New App" → "From scratch"
   - OR select an existing app
3. **Enable Incoming Webhooks**:
   - In the left sidebar, click **"Incoming Webhooks"**
   - Toggle **"Activate Incoming Webhooks"** to **ON**
4. **Add Webhook to Workspace**:
   - Click **"Add New Webhook to Workspace"**
   - **Select the channel** where you want alerts (e.g., `#alerts`, `#devops`)
   - Click **"Allow"**
5. **Copy the Webhook URL**:
   - You'll see a webhook URL like: `https://hooks.slack.com/services/*`
   - Copy this URL

**That's it!** No Workflow Builder or JSON configuration needed. The local collector sends formatted JSON automatically.

### Option 2: Workflow Builder (Advanced)

If you prefer using Workflow Builder:

1. **Go to Slack Workflow Builder**: https://slack.com/workflows
2. **Create a new workflow** → "From a webhook"
3. **Configure the webhook trigger**:
   - Copy the webhook URL provided
   - This is your `SLACK_WEBHOOK_URL`
4. **Add steps** (optional):
   - You can add steps to format/transform the message
   - But the local collector already sends well-formatted JSON, so this is usually unnecessary
5. **Set the channel** where alerts should post

**Note**: For this use case, **Option 1 (Incoming Webhooks) is recommended** because:
- Simpler setup (no workflow steps needed)
- The local collector already sends properly formatted JSON
- Less moving parts = more reliable

### Important Notes

- **No JSON configuration needed**: Whether using Incoming Webhooks or Workflow Builder, you don't need to configure JSON structure
- **Channel selection is enough**: Just selecting the channel is sufficient
- **The local collector sends the JSON**: Our code automatically formats and sends the JSON payload with all mesh metrics

### What Slack Shows You

If using Incoming Webhooks, Slack might show you an example JSON like:
```json
{
  "text": "Hello, World!"
}
```

**You can ignore this** - it's just an example. The local collector will send its own formatted JSON with all the mesh metrics automatically.

## JSON Format Being Sent

The local collector automatically sends messages in Slack's webhook format. Here's the JSON structure:

```json
{
  "username": "Local Collector",
  "icon_emoji": ":satellite_antenna:",
  "attachments": [
    {
      "color": "danger",
      "title": "🚨 Gossipsub Mesh Alert: mesh_state_transition:healthy->pruned",
      "text": "*State:* pruned\n*Severity:* CRITICAL",
      "fields": [
        {
          "title": "Mesh State",
          "value": "pruned",
          "short": true
        },
        {
          "title": "Severity",
          "value": "CRITICAL",
          "short": true
        },
        {
          "title": "Discovery Peers",
          "value": "0",
          "short": true
        },
        {
          "title": "Submissions Peers",
          "value": "0",
          "short": true
        },
        {
          "title": "Total Connected",
          "value": "0",
          "short": true
        },
        {
          "title": "Consecutive Low",
          "value": "784",
          "short": true
        },
        {
          "title": "Total Pruning Events",
          "value": "1",
          "short": true
        },
        {
          "title": "Last Pruning",
          "value": "2025-12-03T09:08:23Z",
          "short": true
        },
        {
          "title": "Uptime",
          "value": "7h 32m",
          "short": true
        },
        {
          "title": "Event",
          "value": "mesh_state_transition:healthy->pruned",
          "short": false
        }
      ],
      "ts": 1701608423,
      "footer": "Local Collector Mesh Monitor"
    }
  ]
}
```

## Color Codes

- **`danger`** (red): Mesh pruned or critical issues
- **`warning`** (yellow): Mesh degraded or warnings
- **`good`** (green): Mesh recovered or healthy state

## Customization (Optional)

If you want to customize the webhook behavior, you can modify `pkgs/service/slack_alerts.go`:

### Change Username
```go
Username: "Your Custom Name",
```

### Change Icon
```go
IconEmoji: ":your_emoji:",
```

### Change Footer
```go
Footer: "Your Custom Footer",
```

### Add Custom Fields
Add to the `fields` array in `SendMeshAlert()`:
```go
{Title: "Custom Field", Value: "Custom Value", Short: true},
```

## Testing

To test the webhook without waiting for an alert:

```bash
curl -X POST -H 'Content-type: application/json' \
  --data '{"text":"Test message from Local Collector"}' \
  YOUR_WEBHOOK_URL
```

## Environment Variable

Set the webhook URL in your environment:

```bash
export SLACK_WEBHOOK_URL=https://hooks.slack.com/services/YOUR/WEBHOOK/URL
```

Or in docker-compose:
```yaml
environment:
  - SLACK_WEBHOOK_URL=${SLACK_WEBHOOK_URL}
```

## Troubleshooting

### No Alerts Received

1. **Check webhook URL is set**:
   ```bash
   echo $SLACK_WEBHOOK_URL
   ```

2. **Check logs for initialization**:
   ```bash
   docker logs <container> 2>&1 | grep -i slack
   ```
   Should see: `"Slack alerts initialized"` or `"Slack webhook URL not configured - Slack alerts disabled"`

3. **Test webhook manually**:
   ```bash
   curl -X POST -H 'Content-type: application/json' \
     --data '{"text":"Test"}' \
     $SLACK_WEBHOOK_URL
   ```

### Alerts Not Triggering

Alerts only trigger for:
- Mesh pruned events
- Zero-peer publish attempts  
- Extended degraded state (10+ consecutive low checks)

Check mesh state in logs:
```bash
docker logs <container> 2>&1 | grep "mesh_state"
```

