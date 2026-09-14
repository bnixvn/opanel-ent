import React from 'react';
import { TerminalCard } from '../AccountAccess.jsx';

// A page of its own rather than a card inside the account settings: a shell
// is a thing somebody comes to the panel to use, not a setting they came to
// change.
export default function TerminalPage() {
  return <TerminalCard />;
}
