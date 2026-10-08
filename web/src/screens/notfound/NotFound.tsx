import { useNavigate } from 'react-router';
import { Bare } from '../../app/Bare';
import { Button, EmptyState } from '../../ds';
import { useDocumentTitle } from '../../lib/documentTitle';

/** Canvas 6l: outside the frame, the empty-state dot grid, one line, one way home. No countdown. */
export function NotFound() {
  useDocumentTitle('Page not found');
  const navigate = useNavigate();
  return (
    <Bare>
      <EmptyState
        title="This page isn’t here"
        action={
          <Button variant="primary" onClick={() => void navigate('/')}>
            Go to Home
          </Button>
        }
      >
        The link may be old, or the page moved.
      </EmptyState>
    </Bare>
  );
}
