import type { ImgHTMLAttributes } from 'react';
import { useAuthenticatedMediaUrl } from '../../hooks/ui/useAuthenticatedMediaUrl';

type AuthenticatedMediaImageProps = Readonly<
  Omit<ImgHTMLAttributes<HTMLImageElement>, 'src' | 'alt'> & {
    source?: string | null;
    /** Required: pass "" for a decorative image. */
    alt: string;
  }
>;

export function AuthenticatedMediaImage({ source, alt, ...props }: AuthenticatedMediaImageProps) {
  const url = useAuthenticatedMediaUrl(source);
  return <img {...props} alt={alt} src={url} />;
}
