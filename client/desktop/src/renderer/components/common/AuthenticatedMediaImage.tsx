import type { ImgHTMLAttributes } from 'react';
import { useAuthenticatedMediaUrl } from '../../hooks/ui/useAuthenticatedMediaUrl';

interface AuthenticatedMediaImageProps extends Omit<ImgHTMLAttributes<HTMLImageElement>, 'src'> {
  source?: string | null;
}

export function AuthenticatedMediaImage({ source, ...props }: AuthenticatedMediaImageProps) {
  const url = useAuthenticatedMediaUrl(source);
  return <img {...props} src={url} />;
}
