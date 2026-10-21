import './JumpNotice.css';

export interface JumpNoticeProps {
  /** The failure text for the current channel, or null while there is none. */
  text: string | null;
}

/**
 * Says why a jump to a message could not finish. Always mounted, so the first
 * message is announced: a live region that appears together with its text is
 * not spoken. Nothing but `{text}` sits inside, so the empty state is `:empty`.
 */
export default function JumpNotice({ text }: Readonly<JumpNoticeProps>) {
  return <output className="jump-notice">{text}</output>;
}
