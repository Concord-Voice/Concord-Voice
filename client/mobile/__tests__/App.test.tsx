import ReactTestRenderer from 'react-test-renderer';
import App from '../App';

test('renders the app shell', async () => {
  let tree!: ReactTestRenderer.ReactTestRenderer;
  await ReactTestRenderer.act(() => {
    tree = ReactTestRenderer.create(<App />);
  });
  expect(tree.root.findByType('Text' as never).props.children).toBe(
    'Concord Voice',
  );
});

test('follows the dark color scheme', async () => {
  const scheme = jest
    .spyOn(require('react-native'), 'useColorScheme')
    .mockReturnValue('dark');
  let tree!: ReactTestRenderer.ReactTestRenderer;
  await ReactTestRenderer.act(() => {
    tree = ReactTestRenderer.create(<App />);
  });
  expect(tree.root.findByType('Text' as never).props.style).toEqual({
    color: '#fff',
  });
  scheme.mockRestore();
});
