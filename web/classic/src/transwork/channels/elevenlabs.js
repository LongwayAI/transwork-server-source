export const TRANSWORK_CHANNEL_OPTIONS = [
  {
    value: 58,
    color: 'purple',
    label: 'ElevenLabs',
  },
];

export function getTransworkChannelModelFallback(type) {
  switch (type) {
    case 58:
      return ['scribe_v2', 'scribe_v2_realtime'];
    default:
      return [];
  }
}

export function getTransworkSecretPrompt(type) {
  switch (type) {
    case 58:
      return '请输入 ElevenLabs API Key';
    default:
      return null;
  }
}
